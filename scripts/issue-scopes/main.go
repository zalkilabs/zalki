// Command issue-scopes attaches literal strict diagnostics to Go declarations.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

type issue struct {
	FromLinter string
	Text       string
	Pos        struct {
		Filename string
		Line     int
	}
}

type scopedIssue struct {
	Linter     string `json:"linter"`
	File       string `json:"file"`
	Scope      string `json:"scope"`
	Text       string `json:"text"`
	Line       int    `json:"line"`
	SourceLine string `json:"source_line"`
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: issue-scopes SOURCE_ROOT STRICT_JSON")
		os.Exit(2)
	}
	rows, err := scopeReport(os.Args[1], os.Args[2])
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(rows)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func scopeReport(root, report string) ([]scopedIssue, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	issues, err := readIssues(report)
	if err != nil {
		return nil, err
	}
	rows := make([]scopedIssue, 0, len(issues))
	for _, finding := range issues {
		row, err := scopeIssue(root, finding)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func readIssues(report string) ([]issue, error) {
	raw, err := os.ReadFile(report) //nolint:gosec // G304: explicit strict-lint artifact supplied to this analysis command.
	if err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	data, ok := envelope["Issues"]
	if !ok || (string(bytes.TrimSpace(data)) != "null" && !bytes.HasPrefix(bytes.TrimSpace(data), []byte("["))) {
		return nil, errors.New("strict JSON must contain an Issues array or explicit null")
	}
	var issues []issue
	if err := json.Unmarshal(data, &issues); err != nil {
		return nil, err
	}
	return issues, nil
}

func issuePath(root, name string) (string, string, error) {
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", errors.New("strict diagnostic is outside source root")
	}
	if err := rejectSourceLinks(root, relative); err != nil {
		return "", "", err
	}
	return path, filepath.ToSlash(relative), nil
}

// Source snapshots do not support links below their root. Do not let an
// ignored/unbound target provide AST declarations or source-line evidence.
func rejectSourceLinks(root, relative string) error {
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == ".git" {
			return errors.New("source metadata is not a Go input")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("source symlinks are not supported")
		}
	}
	return nil
}

func readSource(root, relative string) ([]byte, error) {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	// os.Root also fences escapes if a component changes after the Lstat check.
	data, readErr := opened.ReadFile(relative)
	return data, errors.Join(readErr, opened.Close())
}

func scopeIssue(root string, finding issue) (scopedIssue, error) {
	if finding.FromLinter == "" || finding.Text == "" || finding.Pos.Filename == "" || finding.Pos.Line < 1 {
		return scopedIssue{}, errors.New("strict diagnostic lacks source position or literal key")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return scopedIssue{}, err
	}
	path, relative, err := issuePath(root, finding.Pos.Filename)
	if err != nil {
		return scopedIssue{}, err
	}
	data, err := readSource(root, relative)
	if err != nil {
		return scopedIssue{}, err
	}
	lines := strings.Split(string(data), "\n")
	if finding.Pos.Line > len(lines) {
		return scopedIssue{}, errors.New("strict diagnostic source line is out of bounds")
	}
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, path, data, parser.SkipObjectResolution)
	if err != nil {
		return scopedIssue{}, err
	}
	scope, err := declarationScope(positions, file, finding.Pos.Line)
	if err != nil {
		return scopedIssue{}, err
	}
	return scopedIssue{
		Linter: finding.FromLinter, File: relative,
		Scope: scope, Text: finding.Text,
		Line: finding.Pos.Line, SourceLine: strings.TrimSpace(lines[finding.Pos.Line-1]),
	}, nil
}

func declarationScope(positions *token.FileSet, file *ast.File, line int) (string, error) {
	for _, declaration := range file.Decls {
		if !containsLine(positions, declaration, line) {
			continue
		}
		switch node := declaration.(type) {
		case *ast.FuncDecl:
			return functionScope(positions, node)
		case *ast.GenDecl:
			return generalScope(positions, node, line), nil
		}
	}
	return "package", nil
}

func containsLine(positions *token.FileSet, node ast.Node, line int) bool {
	return positions.Position(node.Pos()).Line <= line && line <= positions.Position(node.End()).Line
}

func functionScope(positions *token.FileSet, function *ast.FuncDecl) (string, error) {
	if function.Recv == nil {
		return function.Name.Name, nil
	}
	if len(function.Recv.List) != 1 {
		return "", errors.New("invalid receiver declaration")
	}
	var receiver bytes.Buffer
	// Printing parsed syntax into bytes.Buffer cannot fail.
	if err := printer.Fprint(&receiver, positions, function.Recv.List[0].Type); err != nil {
		return "", err
	}
	return "(" + receiver.String() + ")." + function.Name.Name, nil
}

func generalScope(positions *token.FileSet, declaration *ast.GenDecl, line int) string {
	for _, specification := range declaration.Specs {
		if !containsLine(positions, specification, line) {
			continue
		}
		switch node := specification.(type) {
		case *ast.TypeSpec:
			return declaration.Tok.String() + " " + node.Name.Name
		case *ast.ValueSpec:
			for _, name := range node.Names {
				return declaration.Tok.String() + " " + name.Name
			}
		}
	}
	return declaration.Tok.String()
}
