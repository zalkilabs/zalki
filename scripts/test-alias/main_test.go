package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAliasRunnerOrchestration(t *testing.T) {
	for _, mode := range []string{"success", "fail-test", "fail-check"} {
		t.Run(mode, func(t *testing.T) { checkAliasRunner(t, mode) })
	}
}

func checkAliasRunner(t *testing.T, mode string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	installFakeGateGo(t, root, mode)
	report := filepath.Join(root, "report.json")
	t.Setenv("ZALKI_TEST_ALIAS_REPORT", report)
	t.Setenv("ZALKI_STATE", "must-not-use")
	t.Setenv("ZALKI_ACTOR", "must-not-use")
	t.Setenv("ZALKI_CAPABILITY", "must-not-use")
	t.Chdir(repo)
	var output, diagnostic bytes.Buffer
	err := run(t.Context(), &output, &diagnostic)
	if (err == nil) != (mode == "success") {
		t.Fatal("runner lost child failure", mode, err, diagnostic.String())
	}
	if mode == "fail-test" && strings.Contains(output.String(), "checker passed") {
		t.Fatal("checker ran after failed tests")
	}
	if _, err := os.Stat(report); err != nil {
		t.Fatal("report was not retained", err)
	}
	rootLine := ""
	for _, line := range strings.Split(output.String(), "\n") {
		if value, ok := strings.CutPrefix(line, "Fixture: "); ok {
			rootLine = value
		}
	}
	if rootLine == "" {
		t.Fatal("fixture was not announced", output.String())
	}
	if _, err := os.Lstat(rootLine); !os.IsNotExist(err) {
		t.Fatal("owned fixture survived completion", rootLine, err)
	}
	if !strings.Contains(output.String(), "GOTMPDIR absent") {
		t.Fatal("alias mode was not announced", output.String())
	}
}

func installFakeGateGo(t *testing.T, root, mode string) {
	t.Helper()
	bin := filepath.Join(root, "toolchain", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{"GOCACHE": filepath.Join(root, "cache"), "GOMODCACHE": filepath.Join(root, "modules"), "GOROOT": filepath.Dir(bin)}
	raw, err := json.Marshal(paths)
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
if [ "${ZAL33_CREDENTIAL_SENTINEL+x}" = x ]; then printf '%s\n' ZAL33_CREDENTIAL_SENTINEL >> '` + filepath.Join(root, "credential-seen") + `'; fi
mode='` + mode + `'
case "$1" in
env) printf '%s\n' '` + string(raw) + `' ;;
build)
 while [ "$1" != -o ]; do shift; done
 target="$2"; package="$3"
 if [ "$mode" = source-change ]; then printf changed >> go.mod; fi
 if [ "$package" = ./scripts/check-integration ]; then
  if [ "$mode" = fail-check ]; then printf '#!/bin/sh\nexit 8\n' > "$target"; else printf '#!/bin/sh\n[ "$1" = --alias ] || exit 9\necho "checker passed"\n' > "$target"; fi
 else printf '#!/bin/sh\nexit 0\n' > "$target"; fi
 chmod 700 "$target" ;;
test)
 [ -z "${GOTMPDIR+x}" ] || exit 11
 [ -L "$TMPDIR" ] || exit 12
 [ -x "$ZALKI_TEST_BINARY" ] || exit 13
 [ -z "${ZALKI_STATE+x}${ZALKI_ACTOR+x}${ZALKI_CAPABILITY+x}" ] || exit 14
 [ -d "$CODEX_HOME" ] && [ -d "$CLAUDE_CONFIG_DIR" ] && [ -d "$CURSOR_CONFIG_DIR" ] || exit 15
 printf '{"Action":"pass","Test":"fixture"}\n'
 [ "$mode" != fail-test ] || exit 7 ;;
*) exit 16 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
}

func TestAliasReportRefusesOverwriteAndOwnedFixture(t *testing.T) {
	f, err := newAliasFixture()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.close(); err != nil {
			t.Error(err)
		}
	}()
	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(existing, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{existing, filepath.Join(f.alias, "report.json")} {
		t.Setenv("ZALKI_TEST_ALIAS_REPORT", path)
		if file, err := newAliasReport(f.root); err == nil {
			file.Close()
			t.Fatal("unsafe evidence path accepted", path)
		}
	}
	raw, err := os.ReadFile(existing)
	if err != nil || string(raw) != "sentinel" {
		t.Fatal("report overwrote sentinel", err)
	}
}

func TestAliasSourceFingerprint(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "source.go")
	if err := os.WriteFile(file, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	before, count, err := sourceDigest(root)
	if err != nil || count != 1 {
		t.Fatal(before, count, err)
	}
	if err := os.Mkdir(filepath.Join(root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "ignored"), []byte("binary"), 0600); err != nil {
		t.Fatal(err)
	}
	stable, _, err := sourceDigest(root)
	if err != nil || stable != before {
		t.Fatal("build artifact changed input fingerprint", err)
	}
	if err := os.Chmod(file, 0700); err != nil {
		t.Fatal(err)
	}
	after, _, err := sourceDigest(root)
	if err != nil || after == before {
		t.Fatal("input mode change missing from fingerprint", err)
	}
}

func TestAliasSourceChangesDuringBuildRefused(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	installFakeGateGo(t, root, "source-change")
	t.Setenv("ZALKI_TEST_ALIAS_REPORT", filepath.Join(root, "report.json"))
	t.Chdir(repo)
	var output, diagnostic bytes.Buffer
	err := run(t.Context(), &output, &diagnostic)
	if err == nil || !strings.Contains(err.Error(), "source_changed") {
		t.Fatal("ALIAS_SOURCE_CHANGE_WAS_CERTIFIED", err)
	}
	if !strings.Contains(output.String(), "checker passed") {
		t.Fatal("source-change fixture never reached checker", diagnostic.String())
	}
}

func TestAliasAmbientCredentialNeverReachesChild(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	installFakeGateGo(t, root, "success")
	const value = "synthetic-credential-value-do-not-echo"
	t.Setenv("ZAL33_CREDENTIAL_SENTINEL", value)
	t.Setenv("GOFLAGS", "-run=should-not-filter-tests")
	t.Setenv("ZALKI_TEST_ALIAS_REPORT", filepath.Join(root, "report.json"))
	t.Chdir(repo)
	var output, diagnostic bytes.Buffer
	err := run(t.Context(), &output, &diagnostic)
	if _, seen := os.Stat(filepath.Join(root, "credential-seen")); !os.IsNotExist(seen) {
		t.Fatal("ALIAS_AMBIENT_CREDENTIAL_REACHED_CHILD")
	}
	if strings.Contains(output.String()+diagnostic.String(), value) {
		t.Fatal("credential value reached diagnostics")
	}
	if err != nil {
		t.Fatal("allowlisted child failed", err, diagnostic.String())
	}
}
