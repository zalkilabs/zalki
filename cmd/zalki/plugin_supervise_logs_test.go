package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// S17: more than 2 MiB through the log leaves exactly the active file and
// one .1 file, each at most 1 MiB and mode 0600; one large write is split.
func TestPluginSupervisorLogRotationCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.log")
	logs, err := openRotatingLog(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 300<<10)
	for range 8 {
		if err := logs.write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := logs.write(bytes.Repeat([]byte("y"), 3<<20)); err != nil {
		t.Fatal(err)
	}
	if err := logs.close(); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{path, path + ".1"} {
		info, err := os.Lstat(file)
		if err != nil || info.Size() > logFileLimit || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: over the cap or not private: %v %v", filepath.Base(file), info, err)
		}
	}
	if _, err := os.Lstat(path + ".2"); !os.IsNotExist(err) {
		t.Fatal("more than one older log was kept", err)
	}
}

// S17: a link, another mode or an unsafe older file is refused and kept;
// after a failed rotation every later write fails.
func TestPluginSupervisorLogRefusesUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(filepath.Join(dir, "target"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := openRotatingLog(link); err == nil {
		t.Fatal("a linked log was opened")
	}
	open := filepath.Join(dir, "open.log")
	if err := os.WriteFile(open, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openRotatingLog(open); err == nil {
		t.Fatal("a log others can read was opened")
	}
	path := filepath.Join(dir, "demo.log")
	if err := os.WriteFile(path+".1", []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	logs, err := openRotatingLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.close()
	if err := logs.write(bytes.Repeat([]byte("x"), logFileLimit+1)); err == nil || logs.failure() == nil {
		t.Fatal("rotation over an unsafe older file did not fail")
	}
	if err := logs.write([]byte("more")); err == nil {
		t.Fatal("a write after a log failure succeeded")
	}
	if raw, _ := os.ReadFile(path + ".1"); string(raw) != "keep" {
		t.Fatal("the unsafe older file was replaced")
	}
}

// S17: the explicit tail shows only the last 50 of 70 lines, across the
// older and the active file, and no line can drive the terminal.
func TestPluginLogsTailIsSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.log")
	var older, active bytes.Buffer
	for i := range 60 {
		older.WriteString("old " + strconv.Itoa(i) + "\n")
	}
	active.WriteString("\x1b[2Jclear\n\u202eflip\n\xffbad\n")
	for i := range 7 {
		active.WriteString("new " + strconv.Itoa(i) + "\n")
	}
	if err := os.WriteFile(path+".1", older.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, active.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailPluginLog(path)
	if err != nil || len(lines) != logTailLines || lines[0] != "old 20" || lines[len(lines)-1] != "new 6" {
		t.Fatalf("tail = %d lines (%v)", len(lines), err)
	}
	for _, line := range lines {
		if shown := plainText(strings.ToValidUTF8(line, "\uFFFD")); strings.Contains(shown, "\x1b") || strings.Contains(shown, "\u202e") || !utf8.ValidString(shown) {
			t.Fatalf("an unsafe line reached the terminal: %q", shown)
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tailPluginLog(path); err == nil {
		t.Fatal("a log others can read was shown")
	}
}
