package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Opt in only through the full alias gate. Its checker refuses a missing or
// skipped proof, while ordinary test runs retain their usual temp environment.
func TestTempAliasEnvironment(t *testing.T) {
	alias := os.Getenv("ZALKI_TEST_ALIAS_ROOT")
	if alias == "" {
		t.Skip("requires make test-alias")
	}
	if _, set := os.LookupEnv("GOTMPDIR"); set {
		t.Fatal("ALIAS_PROOF_GOTMPDIR_MUST_BE_ABSENT")
	}
	if os.Getenv("TMPDIR") != alias {
		t.Fatal("ALIAS_PROOF_TMPDIR_MISMATCH")
	}
	dir := t.TempDir()
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, alias+string(filepath.Separator)) || physical == dir {
		t.Fatal("ALIAS_PROOF_NOT_EXERCISED", dir, physical)
	}
	lexicalInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	physicalInfo, err := os.Stat(physical)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(lexicalInfo, physicalInfo) {
		t.Fatal("ALIAS_PROOF_IDENTITY_MISMATCH")
	}
	t.Logf("actual testing.TempDir alias=%s physical=%s", dir, physical)
}
