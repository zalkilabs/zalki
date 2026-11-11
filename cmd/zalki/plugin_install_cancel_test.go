package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// typedInput gives typed parts in order; the part "block" waits like a
// terminal where nobody types (until the test ends); the part "^C" cancels the command
// context instead, as SIGINT does through signal.NotifyContext in main, and
// that read returns no data. Parts after it are what a person types next. A part
// "^C:text" cancels and returns text in the same read (both arrive together).
type typedInput struct {
	parts   []string
	cancel  func()
	ended   chan struct{} // closed when the test ends; frees a "block" read
	blocked chan struct{} // closed when a "block" read starts waiting
}

func (in *typedInput) Read(p []byte) (int, error) {
	if len(in.parts) > 0 && in.parts[0] == "block" {
		close(in.blocked)
		<-in.ended
		return 0, io.EOF
	}
	if len(in.parts) > 0 && in.parts[0] == "^C" {
		in.cancel()
		in.parts = in.parts[1:]
		return 0, nil
	}
	if len(in.parts) > 0 && strings.HasPrefix(in.parts[0], "^C:") {
		in.cancel()
		in.parts[0] = strings.TrimPrefix(in.parts[0], "^C:")
	}
	if len(in.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, in.parts[0])
	if in.parts[0] = in.parts[0][n:]; in.parts[0] == "" {
		in.parts = in.parts[1:]
	}
	return n, nil
}

// pluginInstallCancelled runs install with a cancellable context and input.
// When cancelAt is set, the command context is cancelled as soon as that text
// is written to the output.
func pluginInstallCancelled(t *testing.T, state string, mode installChoice, ctx context.Context, in io.Reader, folder string, cancelAt ...string) (string, error) {
	t.Helper()
	previous := installTerminal
	installTerminal = func(io.Reader) (installChoice, func()) { return mode, func() {} }
	t.Cleanup(func() { installTerminal = previous })
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := newRootCommand(t.TempDir())
	output := &cappedOutput{limit: 4 << 20}
	if len(cancelAt) == 1 {
		output.watch, output.onWatch = cancelAt[0], cancel
	}
	cmd.SetIn(in)
	cmd.SetOut(output)
	cmd.SetErr(output)
	cmd.SetArgs([]string{"--state", state, "--log-level", "off", "plugins", "install", folder})
	err := cmd.ExecuteContext(ctx)
	return output.String(), err
}

// fakeGoFolder is a Go plugin folder whose "go" is a script on a private PATH.
func fakeGoFolder(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	manifest := `{"name":"demo","language":"go","purpose":"cancel test","build":["go","build","-o","plugin","."],"artifact":"plugin","requests":{"feed":true,"read":true,"send":false,"inbox_read":false,"accept_from":false}}`
	return writeInstallFolder(t, manifest, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
}

func assertInstallCancelled(t *testing.T, state, output string, err error) {
	t.Helper()
	if err != nil || !strings.Contains(output, "Cancelled. Nothing was installed.") || strings.Contains(output, "Installed and enabled") || strings.Contains(output, "build failed") {
		t.Fatalf("not cancelled cleanly: %v\n%s", err, output)
	}
	assertNothingInstalled(t, state)
}

// Ctrl-C at any prompt ends the install; typing more after it changes nothing.
func TestPluginInstallCtrlCAtPromptsCancels(t *testing.T) {
	cases := []struct {
		name    string
		goTool  bool
		mode    installChoice
		parts   []string
		prompts int // session prompts shown: none may follow the cancel
	}{
		{"native, line mode, first prompt", false, installPlainLines, []string{"^C", "1\n\nn\ny\n"}, 1},
		{"native, line mode, later prompt", false, installPlainLines, []string{"1\n", "^C", "\nn\ny\n"}, 2},
		{"native, line mode, final question", false, installPlainLines, []string{"1\n\nn\n", "^C", "y\n"}, 2},
		{"native, line mode, Ctrl-C with the answer", false, installPlainLines, []string{"1\n\nn\n", "^C:y\n"}, 2},
		{"native, raw keys", false, installRawKeys, []string{" ", "^C", "y"}, 0},
		{"native, line mode, nobody types after Ctrl-C", false, installPlainLines, []string{"1\n", "block"}, 2},
		{"go, line mode", true, installPlainLines, []string{"1\n", "^C", "\ny\n"}, 2},
		{"go, raw keys", true, installRawKeys, []string{" ", "^C", "y"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, _ := pluginOwnerFixture(t)
			folder := nativeInstallFolder(t)
			if c.goTool {
				folder = fakeGoFolder(t, `printf built > plugin; chmod 700 plugin`)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			start := time.Now()
			ended := make(chan struct{})
			defer close(ended)
			input := &typedInput{parts: c.parts, cancel: cancel, ended: ended, blocked: make(chan struct{})}
			go func() { // Ctrl-C while the read waits and nobody types
				select {
				case <-input.blocked:
					cancel()
				case <-ended:
				}
			}()
			output, err := pluginInstallCancelled(t, store.Dir, c.mode, ctx, input, folder)
			assertInstallCancelled(t, store.Dir, output, err)
			if strings.Contains(output, "Building...") || strings.Contains(output, "Verifying...") || strings.Count(output, "(enter session numbers") != c.prompts || time.Since(start) > 3*time.Second {
				t.Fatalf("the install went on after Ctrl-C (%s):\n%s", time.Since(start), output)
			}
		})
	}
}

// A cancel that lands after yes stops before the build and before the commit.
func TestPluginInstallCancelBeforeBuildAndCommit(t *testing.T) {
	for _, step := range []string{"build", "commit"} {
		t.Run(step, func(t *testing.T) {
			store, _ := pluginOwnerFixture(t)
			folder := nativeInstallFolder(t)
			if step == "build" {
				folder = fakeGoFolder(t, `printf built > plugin; chmod 700 plugin`)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			installStep = func(at string) {
				if at == step {
					cancel()
				}
			}
			defer func() { installStep = func(string) {} }()
			output, err := pluginInstallCancelled(t, store.Dir, installRawKeys, ctx, strings.NewReader(" y"), folder)
			assertInstallCancelled(t, store.Dir, output, err)
			// "Enabling..." comes before the last check, so it may show; the build may not.
			if strings.Contains(output, "Building...") || (step == "build" && strings.Contains(output, "Enabling...")) {
				t.Fatal("work started after the cancel", output)
			}
		})
	}
}

// Ctrl-C during the build stops it and keeps no build log.
func TestPluginInstallCancelDuringBuildKeepsNoLog(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	mark := filepath.Join(t.TempDir(), "build-started")
	t.Setenv("ZALKI_TEST_BUILD_MARK", mark)
	folder := fakeGoFolder(t, `: > "$ZALKI_TEST_BUILD_MARK"; exec sleep 30`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(mark); err == nil {
				cancel()
				return
			}
		}
	}()
	start := time.Now()
	output, err := pluginInstallCancelled(t, store.Dir, installRawKeys, ctx, strings.NewReader(" y"), folder)
	assertInstallCancelled(t, store.Dir, output, err)
	if !strings.Contains(output, "Building...") || time.Since(start) > 15*time.Second {
		t.Fatal("the build was not stopped by the cancel", time.Since(start), output)
	}
}

type countingReader struct{ calls atomic.Int32 }

func (r *countingReader) Read([]byte) (int, error) {
	r.calls.Add(1)
	return 0, io.EOF
}

// After a cancel the reader starts no new read, so it takes no more keys from
// the terminal (they belong to the shell once zalki has stopped).
func TestContextReaderStopsReadingAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	source := &countingReader{}
	if _, err := newContextReader(ctx, source).Read(make([]byte, 8)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // a wrongly started read runs in its own goroutine
	if n := source.calls.Load(); n != 0 {
		t.Fatalf("read %d time(s) after the cancel", n)
	}
}

// A cancel that lands while "Enabling..." is printed still stops the install:
// the last check comes after all output, right before the commit.
func TestPluginInstallCancelAtEnablingOutput(t *testing.T) {
	for _, goTool := range []bool{false, true} {
		store, _ := pluginOwnerFixture(t)
		folder := nativeInstallFolder(t)
		if goTool {
			folder = fakeGoFolder(t, `printf built > plugin; chmod 700 plugin`)
		}
		output, err := pluginInstallCancelled(t, store.Dir, installRawKeys, t.Context(), strings.NewReader(" y"), folder, "Enabling...")
		assertInstallCancelled(t, store.Dir, output, err)
		if !strings.Contains(output, "Enabling...") {
			t.Fatal("the cancel did not come from the Enabling output", output)
		}
	}
}

// cancelAtSelect cancels its context when Done is asked for, that is while the
// reader's select is set up: Err was still nil just before, so a waiting
// result and the cancel are both ready at once.
type cancelAtSelect struct {
	context.Context
	cancel context.CancelFunc
}

func (c cancelAtSelect) Done() <-chan struct{} {
	c.cancel()
	return c.Context.Done()
}

// Input that is already read when the cancel comes is never used. The select
// may pick either ready case, so it is tried many times (a miss is 2^-64).
func TestContextReaderDropsCompletedInputAtCancel(t *testing.T) {
	for range 64 {
		ctx, cancel := context.WithCancel(t.Context())
		reader := newContextReader(cancelAtSelect{ctx, cancel}, strings.NewReader("unused"))
		reader.pending = true
		reader.results <- contextRead{data: []byte("y\n")}
		n, err := reader.Read(make([]byte, 16))
		cancel()
		if n != 0 || !errors.Is(err, context.Canceled) {
			t.Fatalf("input read before the cancel was used: n=%d err=%v", n, err)
		}
	}
}
