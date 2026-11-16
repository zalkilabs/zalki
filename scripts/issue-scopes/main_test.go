package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiteralScopeSurvivesLineMovement(t *testing.T) {
	root := t.TempDir()
	for _, prefix := range []string{"", "// shifted\n// source\n"} {
		source := prefix + "package example\ntype Store struct{}\nfunc (s *Store) Message() {\n println(1)\n}\n"
		line := strings.Count(prefix, "\n") + 4
		if err := os.WriteFile(filepath.Join(root, "example.go"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		finding := issue{FromLinter: "funlen", Text: "literal diagnostic"}
		finding.Pos.Filename, finding.Pos.Line = "example.go", line
		row, err := scopeIssue(root, finding)
		if err != nil {
			t.Fatal(err)
		}
		if row.Scope != "(*Store).Message" || row.File != "example.go" || row.Linter != "funlen" || row.Text != "literal diagnostic" || row.Line != line || row.SourceLine != "println(1)" {
			t.Fatalf("scope identity changed: %+v", row)
		}
	}
}

func TestStrictReportRequiresAnExplicitIssueCollection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "strict.json")
	for _, input := range []string{"", "{", "{}", "null", `{"Issues":{}}`, `{"Issues":"empty"}`} {
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readIssues(path); err == nil {
			t.Fatalf("malformed strict report accepted: %s", input)
		}
	}
	for _, input := range []string{`{"Issues":null}`, `{"Issues":[]}`} {
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		rows, err := scopeReport(root, path)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(rows)
		if err != nil || string(encoded) != "[]" {
			t.Fatalf("empty result is not an explicit array: %s, %v", encoded, err)
		}
	}
}

func TestStrictScopeRejectsMissingEscapedAndOutOfBoundsSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "example.go"), []byte("package example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		path string
		line int
	}{{"../outside.go", 1}, {"missing.go", 1}, {"example.go", 0}, {"example.go", 900}, {"", 1}} {
		finding := issue{FromLinter: "funlen", Text: "diagnostic"}
		finding.Pos.Filename, finding.Pos.Line = sample.path, sample.line
		if _, err := scopeIssue(root, finding); err == nil {
			t.Fatalf("invalid source accepted: %+v", sample)
		}
	}
}

func TestStrictScopeRejectsSourceSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	const source = "package external\nfunc OutsideOnly() {}\n"
	outsideFile := filepath.Join(outside, "outside.go")
	if err := os.WriteFile(outsideFile, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "inside.go")
	if err := os.WriteFile(inside, []byte("package internal\nfunc InsideOnly() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct{ name, target, input string }{
		{"file.go", outsideFile, "file.go"},
		{"directory", outside, filepath.Join("directory", "outside.go")},
		{"inside-link.go", inside, "inside-link.go"},
	} {
		if err := os.Symlink(link.target, filepath.Join(root, link.name)); err != nil {
			t.Fatal(err)
		}
		finding := issue{FromLinter: "funlen", Text: "literal diagnostic"}
		finding.Pos.Filename, finding.Pos.Line = link.input, 2
		row, err := scopeIssue(root, finding)
		if err == nil || !strings.Contains(err.Error(), "source symlinks are not supported") {
			t.Fatalf("linked AST/source evidence accepted: row=%+v, error=%v", row, err)
		}
	}
	// A regular root-owned source still supplies its exact declaration/text.
	finding := issue{FromLinter: "funlen", Text: "literal diagnostic"}
	finding.Pos.Filename, finding.Pos.Line = "inside.go", 2
	row, err := scopeIssue(root, finding)
	if err != nil || row.Scope != "InsideOnly" || row.SourceLine != "func InsideOnly() {}" {
		t.Fatalf("regular source rejected or misbound: %+v, %v", row, err)
	}
}
