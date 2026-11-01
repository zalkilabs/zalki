package bus

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var clientNames = []string{"claude", "codex", "cursor"}

// Pagination uses "cursor" in its ordinary sense, not the client. These are
// the only places core may spell it, and only in these exact forms.
var paginationCursor = map[string]map[string]bool{
	"inbox.go": {"Cursor": true, "cursor": true},
	"tools.go": {"Cursor": true, `"cursor"`: true},
}

// TestCoreHasNoClientSpecificCode keeps client behavior in the adapter
// packages: core source must not import them, name a client in an identifier,
// or spell a client's name or address in a string. Comments and tests are
// exempt; core tests use stub adapters named after real clients.
func TestCoreHasNoClientSpecificCode(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		allowed := paginationCursor[path]
		for _, spec := range file.Imports {
			if strings.Contains(spec.Path.Value, "/internal/adapters") {
				t.Errorf("%s imports adapter package %s", fset.Position(spec.Pos()), spec.Path.Value)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.Ident:
				if namesClient(strings.ToLower(n.Name), strings.HasPrefix) && !allowed[n.Name] {
					t.Errorf("%s: identifier %s names a client", fset.Position(n.Pos()), n.Name)
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING || allowed[n.Value] {
					return true
				}
				value, err := strconv.Unquote(n.Value)
				if err != nil {
					value = n.Value
				}
				if literalNamesClient(value) {
					t.Errorf("%s: string %s names a client", fset.Position(n.Pos()), n.Value)
				}
			}
			return true
		})
	}
}

func namesClient(s string, match func(string, string) bool) bool {
	for _, name := range clientNames {
		if match(s, name) {
			return true
		}
	}
	return false
}

// literalNamesClient matches a client's proper name, its lowercase kind, or an
// address in that kind. Lowercase "cursor" inside a longer word, such as
// "next_cursor", is pagination.
func literalNamesClient(value string) bool {
	for _, name := range clientNames {
		proper := strings.ToUpper(name[:1]) + name[1:]
		if strings.Contains(value, proper) || value == name || strings.Contains(value, name+":") || strings.Contains(value, name+"-") {
			return true
		}
	}
	return strings.Contains(value, "claude") || strings.Contains(value, "codex")
}
