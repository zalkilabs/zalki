package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A renamed, moved or deleted test whose row stays in the ledger makes CI fail
// with a missing run/pass. Catch it here, where the author runs it.
func TestRequiredRowsNameExistingTestFunctions(t *testing.T) {
	for _, row := range requiredForMode(true) {
		dir := filepath.Join("..", "..", strings.TrimPrefix(row.importPath(), "github.com/zalkilabs/zalki/"))
		if !hasTestFunction(t, dir, row.name) {
			t.Errorf("required row %s %s has no top-level test function", row.pkg, row.name)
		}
	}
}

func TestHasTestFunctionFindsOnlyTopLevelTests(t *testing.T) {
	dir := t.TempDir()
	source := "package x\nimport \"testing\"\nfunc TestTop(t *testing.T) {}\ntype s struct{}\nfunc (s) TestMethod(t *testing.T) {}\nfunc helper() { _ = func() {} }\n"
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("func TestText("), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"TestTop": true, "TestMethod": false, "helper": true, "TestText": false, "TestMissing": false} {
		if got := hasTestFunction(t, dir, name); got != want {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
	}
}

func hasTestFunction(t *testing.T, dir, name string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				return true
			}
		}
	}
	return false
}
