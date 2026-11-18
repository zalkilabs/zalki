package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

func withinMutation(fset *token.FileSet, start, end token.Pos, offset int, old string) bool {
	trimmed := strings.TrimSpace(old)
	at := offset + strings.Index(old, trimmed)
	return fset.Position(start).Offset <= at && at+len(trimmed) <= fset.Position(end).Offset
}

func valueMutationMatches(fset *token.FileSet, decl *ast.GenDecl, r mutationRequest, offset int) bool {
	wanted := token.CONST
	if r.Kind == "var" {
		wanted = token.VAR
	}
	if decl.Tok != wanted {
		return false
	}
	for _, item := range decl.Specs {
		value, ok := item.(*ast.ValueSpec)
		if !ok || len(value.Names) != 1 || value.Names[0].Name != r.Symbol {
			continue
		}
		start, end := value.Pos(), value.End()
		if value.Comment != nil {
			end = value.Comment.End()
		}
		if !decl.Lparen.IsValid() && len(decl.Specs) == 1 {
			start = decl.Pos()
		}
		if withinMutation(fset, start, end, offset, r.Old) {
			return true
		}
	}
	return false
}

const accessFieldMutationOld = "type PluginAccess struct {"
const accessFieldMutationNew = "type PluginAccess struct {\n\tUnclassified bool `json:\"unclassified,omitempty\"`"

func closedAccessFieldMutation(r mutationRequest) bool {
	return r.File == "internal/pluginfeed/plugin_access.go" && r.Symbol == "PluginAccess" &&
		r.Old == accessFieldMutationOld && r.New == accessFieldMutationNew && r.Count == 1
}

func accessFieldDeclaration(fset *token.FileSet, decl ast.Decl, r mutationRequest, offset int) bool {
	group, ok := decl.(*ast.GenDecl)
	if !ok || group.Tok != token.TYPE || group.Lparen.IsValid() || len(group.Specs) != 1 {
		return false
	}
	spec, ok := group.Specs[0].(*ast.TypeSpec)
	if !ok || spec.Name.Name != r.Symbol || spec.Assign.IsValid() || spec.TypeParams != nil {
		return false
	}
	structure, ok := spec.Type.(*ast.StructType)
	return ok && structure.Fields != nil && fset.Position(group.Pos()).Offset == offset &&
		fset.Position(structure.Fields.Opening).Offset+1 == offset+len(r.Old)
}

func declarationMatches(fset *token.FileSet, decl ast.Decl, r mutationRequest, offset int) bool {
	if r.Kind == "type" {
		return accessFieldDeclaration(fset, decl, r, offset)
	}
	if r.Kind == "const" || r.Kind == "var" {
		value, ok := decl.(*ast.GenDecl)
		return ok && valueMutationMatches(fset, value, r, offset)
	}
	function, ok := decl.(*ast.FuncDecl)
	return ok && function.Name.Name == r.Symbol && function.Body != nil &&
		withinMutation(fset, function.Body.Pos(), function.Body.End(), offset, r.Old)
}

func mutationDeclaration(raw []byte, r mutationRequest, offset int) error {
	if r.Kind != "" && r.Kind != "function" && r.Kind != "const" && r.Kind != "var" && r.Kind != "type" {
		return errors.New("unknown mutation declaration kind")
	}
	if r.Kind == "type" && !closedAccessFieldMutation(r) {
		return errors.New("unreviewed access field mutation")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, r.File, raw, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	if r.Kind == "type" && file.Name.Name != "pluginfeed" {
		return errors.New("access field mutation package differs")
	}
	count := 0
	for _, decl := range file.Decls {
		if declarationMatches(fset, decl, r, offset) {
			count++
		}
	}
	if count != 1 {
		return errors.New("mutation preimage is outside its reviewed declaration")
	}
	return nil
}
