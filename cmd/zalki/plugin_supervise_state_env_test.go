package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginSuperviseStateFollowsEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Chdir(home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	state, xdg, flag := filepath.Join(home, "env-state"), filepath.Join(home, "xdg"), filepath.Join(home, "flag-state")
	for _, tc := range []struct {
		name, zalki, xdg, flag, config, want string
		given, refuse                        bool
	}{
		{name: "default", want: filepath.Join(home, ".local", "state", "zalki")},
		{name: "absolute XDG", xdg: xdg, want: filepath.Join(xdg, "zalki")},
		{name: "Zalki before XDG", zalki: state, xdg: xdg, want: state},
		{name: "relative Zalki", zalki: "relative-state", want: filepath.Join(cwd, "relative-state")},
		{name: "relative XDG ignored", xdg: "relative-xdg", want: filepath.Join(home, ".local", "state", "zalki")},
		{name: "explicit before environment", zalki: state, xdg: xdg, flag: flag, given: true, want: flag},
		{name: "relative explicit", zalki: state, flag: "relative-flag", given: true, want: filepath.Join(cwd, "relative-flag")},
		{name: "invalid environment state", zalki: "bad\nstate", refuse: true},
		{name: "invalid common config path", zalki: state, config: "bad\nconfig", refuse: true},
		{name: "explicit keeps its existing config independence", flag: flag, given: true, config: "bad\nconfig", want: flag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZALKI_STATE", tc.zalki)
			t.Setenv("XDG_STATE_HOME", tc.xdg)
			t.Setenv("ZALKI_CONFIG", tc.config)
			t.Setenv("XDG_CONFIG_HOME", "")
			got, err := superviseState(home, tc.flag, tc.given)
			if tc.refuse {
				if err == nil || model.ErrorCodeOf(err) != model.CodeInvalidInput {
					t.Fatalf("invalid supervise path was accepted: state=%q err=%v", got, err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("supervise selected %q (%v), want %q", got, err, tc.want)
			}
		})
	}
	t.Run("clean HOME still required", func(t *testing.T) {
		for _, badHome := range []string{"", "relative-home", home + "/../unclean"} {
			t.Setenv("HOME", badHome)
			err := runSupervisor(t.Context(), flag, true)
			if err == nil || model.ErrorCodeOf(err) != model.CodeInvalidInput || !strings.Contains(err.Error(), "clean absolute HOME") {
				t.Fatal("supervise lost its clean HOME refusal", err)
			}
		}
	})
}

// The selected private fixture has an unsafe plugins link; every other state
// has no plugins directory. Its refusal therefore proves the real CLI routed
// through the selected state, without starting a controller or plugin process.
func TestPluginSuperviseEnvironmentSelectionCLI(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the freshly built zalki CLI")
	}
	for _, source := range []string{"ZALKI_STATE", "relative ZALKI_STATE", "XDG_STATE_HOME", "explicit"} {
		t.Run(source, func(t *testing.T) {
			checkSuperviseEnvironmentCLI(t, binary, source)
		})
	}
}

func checkSuperviseEnvironmentCLI(t *testing.T, binary, source string) {
	t.Helper()
	home, cwd, target := t.TempDir(), t.TempDir(), t.TempDir()
	state := filepath.Join(cwd, "selected-state", "zalki")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(state, "plugins")); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, "settings.json")
	if err := os.WriteFile(settings, []byte("not JSON: supervise must not load it"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"plugins", "supervise"}
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "ZALKI_CONFIG=" + settings, "ZALKI_LOG_LEVEL=not-a-level"}
	switch source {
	case "ZALKI_STATE":
		env = append(env, "ZALKI_STATE="+state, "XDG_STATE_HOME="+filepath.Join(cwd, "unused-xdg"))
	case "relative ZALKI_STATE":
		env = append(env, "ZALKI_STATE="+filepath.Join("selected-state", "zalki"))
	case "XDG_STATE_HOME":
		env = append(env, "XDG_STATE_HOME="+filepath.Dir(state))
	case "explicit":
		args = append([]string{"--state", state}, args...)
		env = append(env, "ZALKI_STATE="+filepath.Join(cwd, "unused-state"), "XDG_STATE_HOME="+filepath.Join(cwd, "unused-xdg"))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = cwd, env
	out, err := cmd.CombinedOutput()
	if err != nil || ctx.Err() != nil || !strings.Contains(string(out), errUnsafePluginsDir.Error()) {
		t.Fatalf("supervise did not refuse the environment-selected unsafe fixture: output=%q err=%v context=%v", out, err, ctx.Err())
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal("supervise wrote through the unsafe selected plugins directory", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "state", "zalki")); !os.IsNotExist(err) {
		t.Fatal("supervise created the unrelated default state", err)
	}
}

func TestPluginSuperviseStateKeepsNotificationBoundary(t *testing.T) {
	home := t.TempDir()
	oldPlatform, oldHome := backgroundPlatform, loginItemAccountHome
	t.Cleanup(func() { backgroundPlatform, loginItemAccountHome = oldPlatform, oldHome })
	backgroundPlatform, loginItemAccountHome = "darwin", func() string { return home }
	for _, tc := range []struct {
		name, state, xdg string
		wantNotifier     bool
	}{
		{name: "default", wantNotifier: true},
		{name: "Zalki default", state: filepath.Join(home, ".local", "state", "zalki"), wantNotifier: true},
		{name: "Zalki custom", state: filepath.Join(home, "custom")},
		{name: "XDG custom", xdg: filepath.Join(home, "xdg")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZALKI_STATE", tc.state)
			t.Setenv("XDG_STATE_HOME", tc.xdg)
			t.Setenv("ZALKI_CONFIG", "")
			t.Setenv("XDG_CONFIG_HOME", "")
			state, err := superviseState(home, "", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(state, "plugins"), 0o700); err != nil {
				t.Fatal(err)
			}
			got := notifierFor(state, home, model.NewUUID())
			closeNotifierAfterTest(t, got)
			if (got != nil) != tc.wantNotifier || tc.wantNotifier && got.dir == nil {
				t.Fatal("environment selection changed default-only notification authority")
			}
		})
	}
}

// No notification worker is started by these selection-only fixtures; the
// test retains ownership of the default-state directory handle it opens.
func closeNotifierAfterTest(t *testing.T, got *notifier) {
	t.Helper()
	if got != nil && got.dir != nil {
		t.Cleanup(func() {
			if err := got.dir.Close(); err != nil {
				t.Error(err)
			}
		})
	}
}
