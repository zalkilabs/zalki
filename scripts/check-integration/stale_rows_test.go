package main

import (
	"go/build"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The module root, seen from this package directory.
const moduleRoot = "../.."

// Every required row names a package with Go files, so a moved or removed
// package fails here and not only after a full required test run.
func TestRequiredRowsNameExistingPackages(t *testing.T) {
	rows := requiredForMode(true)
	if len(rows) == 0 {
		t.Fatal("no required rows to check")
	}
	if missing := missingPackages(moduleRoot, rows); len(missing) > 0 {
		t.Fatalf("required rows name packages that do not exist: %s", strings.Join(missing, ", "))
	}
}

func TestMissingPackagesFindsStaleRows(t *testing.T) {
	rows := []requiredTest{
		{"bus", "TestA"},
		{"cmd/zalki", "TestB"},
		{"gone", "TestC"},
		{"gone", "TestD"},
		{"cmd/gone", "TestE"},
		{"adapters/gone", "TestF"},
		{"bus/testdata", "TestG"},
	}
	want := []string{"gone", "cmd/gone", "adapters/gone", "bus/testdata"}
	if got := missingPackages(moduleRoot, rows); !slices.Equal(got, want) {
		t.Fatalf("missing packages = %v, want %v", got, want)
	}
}

// missingPackages returns each row package, once, that has no Go files under root.
func missingPackages(root string, tests []requiredTest) []string {
	var missing []string
	for _, test := range tests {
		rel := strings.TrimPrefix(test.importPath(), "github.com/zalkilabs/zalki/")
		if _, err := build.ImportDir(filepath.Join(root, filepath.FromSlash(rel)), 0); err != nil && !slices.Contains(missing, test.pkg) {
			missing = append(missing, test.pkg)
		}
	}
	return missing
}
