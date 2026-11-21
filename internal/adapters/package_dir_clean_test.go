package adapters_test

import (
	"fmt"
	"os"
	"slices"
	"testing"
)

// A test run must leave the package folder as it found it. A real client run by
// diagnostics with an invalid profile once left config files here.
func TestMain(m *testing.M) {
	before := folderEntries()
	code := m.Run()
	if left := newEntries(before, folderEntries()); len(left) > 0 {
		fmt.Fprintln(os.Stderr, "tests left files in the package folder:", left)
		code = max(code, 1)
	}
	os.Exit(code)
}

func folderEntries() []string {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func newEntries(before, after []string) []string {
	var added []string
	for _, name := range after {
		if !slices.Contains(before, name) {
			added = append(added, name)
		}
	}
	return added
}

func TestNewEntriesReportsLeftFiles(t *testing.T) {
	if got := newEntries([]string{"a"}, []string{"a", ".cursor"}); len(got) != 1 || got[0] != ".cursor" {
		t.Fatal("a new entry was not reported", got)
	}
	if got := newEntries([]string{"a", "b"}, []string{"a"}); len(got) != 0 {
		t.Fatal("a removed entry was reported", got)
	}
}
