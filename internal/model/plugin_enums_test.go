package model

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// pluginEnumKinds maps a constant's declared type (or, for untyped event
// types, its name prefix) to its section in testdata/plugin-enums.json.
var pluginEnumKinds = map[string]string{
	"SessionState":   "session_state",
	"DeliveryStatus": "delivery_status",
	"EventCause":     "event_cause",
}

// sourcePluginEnums reads every enum value a plugin can see from this
// package's source, so the published list is never typed by hand.
func sourcePluginEnums(t *testing.T) map[string][]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string][]string{"message_kind": {""}}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			collectEnumDecl(t, decl, found)
		}
	}
	for kind := range found {
		slices.Sort(found[kind])
	}
	return found
}

func collectEnumDecl(t *testing.T, decl ast.Decl, found map[string][]string) {
	t.Helper()
	gen, ok := decl.(*ast.GenDecl)
	if !ok || gen.Tok != token.CONST {
		return
	}
	for _, spec := range gen.Specs {
		value := spec.(*ast.ValueSpec)
		for i, name := range value.Names {
			kind := enumKind(value, name.Name)
			if kind == "" || len(value.Values) <= i {
				continue
			}
			if text, ok := stringLiteral(t, value.Values[i]); ok {
				found[kind] = append(found[kind], text)
			}
		}
	}
}

// enumKind names the published section of a constant, or "".
func enumKind(value *ast.ValueSpec, name string) string {
	if ident, ok := value.Type.(*ast.Ident); ok && pluginEnumKinds[ident.Name] != "" {
		return pluginEnumKinds[ident.Name]
	}
	switch {
	case value.Type == nil && strings.HasPrefix(name, "Event"):
		return "event_type"
	case name == "NoticeKind":
		return "message_kind"
	}
	return ""
}

func stringLiteral(t *testing.T, expr ast.Expr) (string, bool) {
	t.Helper()
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(literal.Value)
	if err != nil {
		t.Fatal(err)
	}
	return text, true
}

// TestPublishedPluginEnums fails when an enum value is added or removed
// without updating testdata/plugin-enums.json, the fixture plugin authors use.
// Every value has a one-sentence meaning. Plugins must tolerate unknown values.
func TestPublishedPluginEnums(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "plugin-enums.json"))
	if err != nil {
		t.Fatal(err)
	}
	var published map[string][]struct {
		Value   string `json:"value"`
		Meaning string `json:"meaning"`
	}
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for kind, entries := range published {
		for _, entry := range entries {
			if strings.TrimSpace(entry.Meaning) == "" {
				t.Errorf("%s %q has no meaning", kind, entry.Value)
			}
			got[kind] = append(got[kind], entry.Value)
		}
		slices.Sort(got[kind])
	}
	if want := sourcePluginEnums(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("testdata/plugin-enums.json is out of date.\npublished: %v\nsource:    %v", got, want)
	}
}
