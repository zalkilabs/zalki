package adapters_test

import (
	"path/filepath"
	"testing"
)

func TestCursorProfileIgnoresARelativeXDGFolderAndKeepsTheExplicitOverrideFirst(t *testing.T) {
	clearProfileEnvironment(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative-xdg")
	if got, want := selectedProfile(t, "cursor"), filepath.Join(home, ".cursor"); got != want {
		t.Fatalf("relative XDG_CONFIG_HOME changed the Cursor profile: got %q, want %q", got, want)
	}
	xdg := t.TempDir()
	override := filepath.Join(t.TempDir(), "explicit")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("CURSOR_CONFIG_DIR", override)
	if got := selectedProfile(t, "cursor"); got != override {
		t.Fatalf("CURSOR_CONFIG_DIR lost to XDG_CONFIG_HOME: got %q, want %q", got, override)
	}
	t.Setenv("CURSOR_CONFIG_DIR", "relative-profile")
	if got := selectedProfile(t, "cursor"); got != "" {
		t.Fatalf("an invalid CURSOR_CONFIG_DIR fell back to XDG_CONFIG_HOME: got %q", got)
	}
}
