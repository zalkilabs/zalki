package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSnapshotRejectsPartialDiscovery(t *testing.T) {
	const complete = `{"ImportPath":"github.com/zalkilabs/zalki/internal/p","Dir":"/fixture","GoFiles":["p.go"]}`
	for _, item := range []struct {
		out, stderr string
		err         error
	}{
		{complete, "", errors.New("discovery failed")},
		{complete, "warning on stderr", nil},
		{complete + "{", "", nil},
	} {
		if _, err := decodePackages([]byte(item.out), item.stderr, item.err); err == nil {
			t.Fatal("partial or diagnostic discovery became a complete snapshot")
		}
	}
	packages, err := decodePackages([]byte(complete), "", nil)
	if err != nil || len(packages) != 1 || packages[0].ImportPath != modulePrefix+"internal/p" {
		t.Fatalf("complete discovery was lost: %v, %v", packages, err)
	}
}

func TestSnapshotUsesTheRepoNamedExportRule(t *testing.T) {
	const source = "package p\ntype Public struct { Value string `json:\"value\"` }\ntype hidden struct{}\nconst Exported, private = 1, 2\nvar Exposed = 3\nfunc (p Public) A() {}\nfunc (p *Public) b() {}\nfunc (h *hidden) C() {}\nfunc Free() {}\n"
	stats, err := analyzeSource("selected.go", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	collector := newSurfaceCollector()
	if err := collector.file("internal/p", "selected.go", []byte(source)); err != nil {
		t.Fatal(err)
	}
	if len(collector.facts.Exports) != stats.Exported || stats.Exported != 6 {
		t.Fatalf("named ledger differs from repo tool: %v vs %d", collector.facts.Exports, stats.Exported)
	}
	if !slices.Contains(collector.facts.Exports, "internal/p.hidden.C") {
		t.Fatal("exported method on a private receiver was dropped")
	}
	if collector.facts.Receivers["internal/p.Public"] != (receiverCount{Methods: 2, Files: 1}) {
		t.Fatal("private/value/pointer methods were not counted")
	}
}

func TestSnapshotUsesSelectedFilesAndRefusesDuplicateDeclarations(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "internal", "p")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{"selected.go": "package p\nfunc Native() {}\n", "other.go": "package p\nfunc OtherOS() {}\n"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pkg := listedPackage{ImportPath: modulePrefix + "internal/p", Dir: directory, GoFiles: []string{"selected.go"}}
	facts, err := snapshotPackages(root, []listedPackage{pkg})
	if err != nil || !slices.Equal(facts.Exports, []string{"internal/p.Native"}) {
		t.Fatalf("unselected file contributed an export: %+v, %v", facts.Exports, err)
	}
	if _, err := snapshotPackages(root, []listedPackage{pkg, pkg}); err == nil {
		t.Fatal("duplicate selected declarations silently overwrote a shape")
	}
}

func TestSnapshotRetainsAliasReceiverAndWireShapeChanges(t *testing.T) {
	measure := func(source string) map[string]string {
		t.Helper()
		collector := newSurfaceCollector()
		if err := collector.file("internal/p", "p.go", []byte(source)); err != nil {
			t.Fatal(err)
		}
		return collector.facts.Shapes
	}
	alias := measure("package p\ntype Public = int\n")
	defined := measure("package p\ntype Public int\n")
	if alias["type:internal/p.Public"] == defined["type:internal/p.Public"] {
		t.Fatal("alias-to-defined type change disappeared")
	}
	first := measure("package p\ntype Public struct { Value string `json:\"one\"` }\nfunc (p *Public) Method() {}\n")
	second := measure("package p\ntype Public struct { Value string `json:\"two\"` }\nfunc (p Public) Method() {}\n")
	if first["type:internal/p.Public"] == second["type:internal/p.Public"] || first["function:internal/p.Public.Method"] == second["function:internal/p.Public.Method"] {
		t.Fatal("wire tag or pointer/value receiver change disappeared")
	}
}

func TestSnapshotNeverReadsLinkedOutsideSource(t *testing.T) {
	root, external := t.TempDir(), t.TempDir()
	outside := filepath.Join(external, "source.go")
	if err := os.WriteFile(outside, []byte("package outside\nfunc Unbound() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "source.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotSource(root, filepath.Join(root, "source.go")); err == nil || !strings.Contains(err.Error(), "links are unsupported") {
		t.Fatalf("outside linked source was accepted: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotSource(root, filepath.Join(root, "linked", "source.go")); err == nil || !strings.Contains(err.Error(), "links are unsupported") {
		t.Fatalf("outside directory link was accepted: %v", err)
	}
}

func TestSnapshotRetainsImplicitConstantGroupContext(t *testing.T) {
	measure := func(source string) string {
		t.Helper()
		collector := newSurfaceCollector()
		if err := collector.file("internal/p", "p.go", []byte(source)); err != nil {
			t.Fatal(err)
		}
		return collector.facts.Shapes["const:internal/p.Published"]
	}
	before := measure("package p\nconst ( private = iota; Published )\n")
	after := measure("package p\nconst ( private = iota + 1; Published )\n")
	if before == "" || before == after {
		t.Fatal("implicit exported constant lost its private initializer context")
	}
}

func TestSnapshotShapeIgnoresDeclarationMovement(t *testing.T) {
	const source = "package p\ntype Public struct { Value string }\nconst ( private = iota; Published )\nfunc Call(value Public) string { return value.Value }\n"
	before, after := newSurfaceCollector(), newSurfaceCollector()
	if err := before.file("internal/p", "p.go", []byte(source)); err != nil {
		t.Fatal(err)
	}
	moved := "package p\n\n// unrelated documentation\n\n" + strings.TrimPrefix(source, "package p\n")
	if err := after.file("internal/p", "p.go", []byte(moved)); err != nil {
		t.Fatal(err)
	}
	for name, shape := range before.facts.Shapes {
		if after.facts.Shapes[name] != shape {
			t.Fatalf("physical declaration movement changed the literal API shape: %s", name)
		}
	}
}

func TestSnapshotRawOSExceptionsAreExactNotWildcardBudgets(t *testing.T) {
	const controls = `files:
 - "$all"
 - "!$test"
 - "!**/scripts/**"
 - "!**/internal/procwatch/*.go"
 - "!**/internal/platform/*.go"
`
	names, err := parseOSSelectors(controls + ` - "!**/internal/p/source.go"`)
	if err != nil || !slices.Equal(names, []string{"internal/p/source.go"}) {
		t.Fatalf("exact source exception was not retained: %v, %v", names, err)
	}
	for _, text := range []string{
		controls + ` - "!**/internal/p/*.go"`,
		controls + ` - "!$test"`,
		strings.Replace(controls, ` - "$all"`, "", 1),
	} {
		if _, err := parseOSSelectors(text); err == nil {
			t.Fatal("wildcard/missing/duplicate selector weakened the exact exception list")
		}
	}
}
