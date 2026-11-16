package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	goprinter "go/printer"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type receiverCount struct {
	Methods int
	Files   int
}

type architectureFacts struct {
	Exports   []string                 `json:"exports"`
	Shapes    map[string]string        `json:"shapes"`
	Receivers map[string]receiverCount `json:"receivers"`
	Imports   []string                 `json:"imports"`
	OSCalls   map[string][]string      `json:"os_calls"`
}

type architectureSnapshot struct {
	Build            buildInfo         `json:"build"`
	Facts            architectureFacts `json:"facts"`
	OSCallExceptions []string          `json:"os_call_exceptions"`
}

type surfaceCollector struct {
	facts       architectureFacts
	names       map[string]bool
	methodFiles map[string]map[string]bool
}

func newSurfaceCollector() *surfaceCollector {
	return &surfaceCollector{
		facts: architectureFacts{Exports: []string{}, Imports: []string{}, Shapes: map[string]string{}, Receivers: map[string]receiverCount{}, OSCalls: map[string][]string{}},
		names: map[string]bool{}, methodFiles: map[string]map[string]bool{},
	}
}

func sourceNode(node ast.Node) (string, error) {
	var out bytes.Buffer
	err := goprinter.Fprint(&out, token.NewFileSet(), node)
	return out.String(), err
}

func (c *surfaceCollector) export(name, kind string, node ast.Node) error {
	if c.names[name] {
		return fmt.Errorf("duplicate selected declaration: %s", name)
	}
	shape, err := sourceNode(node)
	if err != nil {
		return err
	}
	c.names[name] = true
	c.facts.Exports = append(c.facts.Exports, name)
	c.facts.Shapes[kind+":"+name] = shape
	return nil
}

func (c *surfaceCollector) function(pkg, file string, node *ast.FuncDecl) error {
	label, shape := pkg+"."+node.Name.Name, ast.Node(node.Type)
	if node.Recv != nil {
		if len(node.Recv.List) != 1 {
			return errors.New("invalid method receiver")
		}
		receiver := receiverName(node.Recv.List[0].Type)
		key := pkg + "." + receiver
		count := c.facts.Receivers[key]
		count.Methods++
		if c.methodFiles[key] == nil {
			c.methodFiles[key] = map[string]bool{}
		}
		c.methodFiles[key][file] = true
		count.Files = len(c.methodFiles[key])
		c.facts.Receivers[key] = count
		label = key + "." + node.Name.Name
		// Include pointer/value receiver syntax in the shape without counting
		// receiver-variable names as a separate exported declaration.
		shape = &ast.FuncType{Params: &ast.FieldList{List: []*ast.Field{{Type: node.Recv.List[0].Type}}}, Results: &ast.FieldList{List: []*ast.Field{{Type: node.Type}}}}
	}
	if !node.Name.IsExported() {
		return nil
	}
	return c.export(label, "function", shape)
}

func (c *surfaceCollector) typeSpec(pkg string, item *ast.TypeSpec) error {
	if !item.Name.IsExported() {
		return nil
	}
	// Include aliases/type parameters, excluding only this declaration's docs.
	shape := &ast.TypeSpec{Name: item.Name, TypeParams: item.TypeParams, Assign: item.Assign, Type: item.Type}
	return c.export(pkg+"."+item.Name.Name, "type", shape)
}

func (c *surfaceCollector) valueSpec(pkg, kind string, item *ast.ValueSpec, group *ast.GenDecl) error {
	var shape ast.Node = item
	if kind == "const" {
		// Implicit values and iota depend on the complete declaration group,
		// including private predecessors. Never report only the exported name.
		shape = &ast.GenDecl{Tok: group.Tok, Lparen: group.Lparen, Specs: group.Specs, Rparen: group.Rparen}
	}
	for _, name := range item.Names {
		if name.IsExported() {
			if err := c.export(pkg+"."+name.Name, kind, shape); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *surfaceCollector) declarations(pkg string, node *ast.GenDecl) error {
	for _, specification := range node.Specs {
		var err error
		switch item := specification.(type) {
		case *ast.TypeSpec:
			err = c.typeSpec(pkg, item)
		case *ast.ValueSpec:
			err = c.valueSpec(pkg, node.Tok.String(), item, node)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *surfaceCollector) file(pkg, file string, raw []byte) error {
	parsed, err := parser.ParseFile(token.NewFileSet(), file, raw, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	for _, declaration := range parsed.Decls {
		switch node := declaration.(type) {
		case *ast.FuncDecl:
			if err := c.function(pkg, file, node); err != nil {
				return err
			}
		case *ast.GenDecl:
			if err := c.declarations(pkg, node); err != nil {
				return err
			}
		}
	}
	return nil
}

func snapshotSource(root, name string) ([]byte, error) {
	relative, err := filepath.Rel(root, name)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("snapshot input is outside the source root")
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == ".git" {
			return nil, errors.New("snapshot input is source metadata")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("snapshot source links are unsupported")
		}
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	data, readErr := opened.ReadFile(filepath.FromSlash(relative))
	return data, errors.Join(readErr, opened.Close())
}

func (c *surfaceCollector) packageFiles(root string, p listedPackage) error {
	pkg := relative(p.ImportPath)
	for _, file := range append(append([]string{}, p.GoFiles...), p.CgoFiles...) {
		path := filepath.Join(p.Dir, file)
		raw, err := snapshotSource(root, path)
		if err != nil {
			return err
		}
		if err := c.file(pkg, file, raw); err != nil {
			return err
		}
	}
	for _, target := range p.Imports {
		if strings.HasPrefix(target, modulePrefix) {
			c.facts.Imports = append(c.facts.Imports, pkg+" -> "+relative(target))
		}
	}
	return nil
}

func snapshotPackages(root string, packages []listedPackage) (architectureFacts, error) {
	collector := newSurfaceCollector()
	for _, p := range packages {
		pkg := relative(p.ImportPath)
		if !strings.HasPrefix(p.ImportPath, modulePrefix) || (!strings.HasPrefix(pkg, "cmd/") && !strings.HasPrefix(pkg, "internal/")) {
			continue
		}
		if err := collector.packageFiles(root, p); err != nil {
			return architectureFacts{}, err
		}
	}
	if len(collector.facts.Exports) == 0 {
		return architectureFacts{}, errors.New("snapshot has no selected cmd/internal exported declarations")
	}
	sort.Strings(collector.facts.Exports)
	sort.Strings(collector.facts.Imports)
	return collector.facts, nil
}

func rawOSImports(root, path string) ([]string, error) {
	raw, err := snapshotSource(root, path)
	if err != nil {
		return nil, err
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), path, raw, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	result := []string{}
	for _, imp := range parsed.Imports {
		name, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		if name == "syscall" || name == "golang.org/x/sys" || strings.HasPrefix(name, "golang.org/x/sys/") {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result, nil
}

func osCallFile(root, path string, entry os.DirEntry, result map[string][]string) error {
	if entry.Type()&os.ModeSymlink != 0 {
		return errors.New("snapshot source links are unsupported")
	}
	if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return nil
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "internal/procwatch/") || strings.HasPrefix(rel, "internal/platform/") {
		return nil
	}
	imports, err := rawOSImports(root, path)
	if len(imports) > 0 {
		result[rel] = imports
	}
	return err
}

func allOSCalls(root string) (map[string][]string, error) {
	result := map[string][]string{}
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return osCallFile(root, path, entry, result)
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func sourceOSExceptions(root string) ([]string, error) {
	raw, err := snapshotSource(root, filepath.Join(root, ".golangci.yml"))
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(raw), "        os-calls:")
	if len(parts) != 2 {
		return nil, errors.New("missing/ambiguous raw OS depguard block")
	}
	block := strings.Split(parts[1], "          list-mode:")
	if len(block) < 2 {
		return nil, errors.New("raw OS depguard block has no list mode")
	}
	return parseOSSelectors(block[0])
}

func parseOSSelectors(block string) ([]string, error) {
	pattern := regexp.MustCompile(`^!\*\*/((?:cmd|internal)/[^"*?\[\]]+\.go)$`)
	controls := map[string]int{"$all": 0, "!$test": 0, "!**/scripts/**": 0, "!**/internal/procwatch/*.go": 0, "!**/internal/platform/*.go": 0}
	names := []string{}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "files:" || strings.HasPrefix(line, "#") {
			continue
		}
		name, err := strconv.Unquote(strings.TrimPrefix(line, "- "))
		if err != nil || !strings.HasPrefix(line, "- ") {
			return nil, errors.New("unsupported raw OS file selector")
		}
		if _, ok := controls[name]; ok {
			controls[name]++
			continue
		}
		match := pattern.FindStringSubmatch(name)
		if match == nil {
			return nil, errors.New("raw OS exceptions must name exact source files")
		}
		names = append(names, match[1])
	}
	for _, count := range controls {
		if count != 1 {
			return nil, errors.New("raw OS selector controls are missing or duplicated")
		}
	}
	sort.Strings(names)
	return names, nil
}

func runSnapshot(ctx context.Context, dir string, out io.Writer) error {
	root, err := filepath.Abs(dir)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return err
	}
	packages, err := listPackages(ctx, root)
	if err != nil {
		return err
	}
	facts, err := snapshotPackages(root, packages)
	if err != nil {
		return err
	}
	facts.OSCalls, err = allOSCalls(root)
	if err != nil {
		return err
	}
	exceptions, err := sourceOSExceptions(root)
	if err != nil {
		return err
	}
	build, err := readBuildInfo(ctx, root)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(architectureSnapshot{Build: build, Facts: facts, OSCallExceptions: exceptions})
}
