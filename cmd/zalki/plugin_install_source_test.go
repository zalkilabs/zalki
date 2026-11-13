package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestPluginInstallUnsafeSourceDiagnostics(t *testing.T) {
	for _, kind := range []string{"raw-missing", "raw-missing-manifest", "alias-missing-manifest", "alias-dangling"} {
		t.Run(kind, func(t *testing.T) { checkUnsafePluginSource(t, kind) })
	}
}

func checkUnsafePluginSource(t *testing.T, kind string) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	root := t.TempDir()
	unsafe := filepath.Join(root, "source\x1b[2J\nFORGED_PATH\u202e")
	folder := unsafe
	if kind == "raw-missing-manifest" || kind == "alias-missing-manifest" {
		if err := os.Mkdir(unsafe, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if strings.HasPrefix(kind, "alias-") {
		folder = filepath.Join(root, "safe-alias")
		if err := os.Symlink(unsafe, folder); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	err := executeInstallTest(t, store.Dir, strings.NewReader("y\n"), &output, folder)
	if err == nil {
		t.Fatal("unsafe source was not refused")
	}
	diagnostic := err.Error()
	if !utf8.ValidString(diagnostic) || strings.ContainsFunc(diagnostic, func(r rune) bool { return !unicode.IsPrint(r) }) {
		t.Fatal("UNSAFE_SOURCE_DIAGNOSTIC_RAW", strconv.QuoteToASCII(diagnostic))
	}
	if kind == "alias-dangling" && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source resolution lost not-exist classification", diagnostic)
	}
	if output.String() != "Preparing...\n" {
		t.Fatal("unsafe source reached staging/build/review output", strconv.QuoteToASCII(output.String()))
	}
	if files := pluginStateFiles(t, store.Dir); len(files) != 0 {
		t.Fatal("unsafe source created staging or install state", files)
	}
}

func TestPluginInstallSourceErrorClassification(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	var output bytes.Buffer
	err := executeInstallTest(t, store.Dir, strings.NewReader(""), &output, filepath.Join(t.TempDir(), "missing"))
	var pathError *os.PathError
	if !errors.Is(err, os.ErrNotExist) || !errors.As(err, &pathError) {
		t.Fatal("safe source error lost filesystem classification", err)
	}
}
