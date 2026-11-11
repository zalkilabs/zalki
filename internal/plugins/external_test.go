package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

type externalEvidence struct {
	Args              []string
	Cwd               string
	Home              string
	EnvironmentCount  int
	SentinelInherited bool
}

func externalTestChild(args []string) error {
	if err := pluginfeed.CheckPluginLeaseFD(3); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	e := externalEvidence{args[1:], cwd, os.Getenv("HOME"), len(os.Environ()), os.Getenv("ZALKI_EXTERNAL_SENTINEL") != ""}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := journal.WriteAtomic(args[0], raw); err != nil {
		return err
	}
	if err := writeFrame(os.Stdout, frame{Hello: &hello{Version: "v0"}}); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(os.Stdin, maxFrame+1)
	for {
		value, err := readFrame(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		raw, err := journal.EncodeJSON(value, maxFrame)
		if err != nil {
			return err
		}
		if err := journal.WriteAtomic(args[0]+".last", raw); err != nil {
			return err
		}
		if err := writeFrame(os.Stdout, frame{Ack: value.ID}); err != nil {
			return err
		}
	}
}

func TestExternalPluginLiteralArgsHomeOnlyAndPersistentCwd(t *testing.T) {
	store, session := hostStore(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "external")
	data := filepath.Join(store.Dir, "plugins", "external.data")
	evidence := filepath.Join(data, "observed.json")
	marker := filepath.Join(data, "shell-was-used")
	literals := []string{"$(/usr/bin/touch " + marker + ")", "$HOME", "literal space", "--not-an-internal-flag"}
	args := append([]string{"external-test-child", evidence}, literals...)
	if err := feed.AddExternalPlugin(binary, args); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginFeed([]string{session.ID}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZALKI_EXTERNAL_SENTINEL", "not-a-real-secret")
	sentinel := filepath.Join(data, "persistent")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	check := externalObservationCheck{store, binary, evidence, marker, data, home, sentinel, literals}
	t.Run("initial", check.run)
	t.Run("restart", check.run)
}

func runExternalUntilObserved(t *testing.T, store bus.Store, binary, evidence string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, store, binary, "external", io.Discard); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("external child was not reaped")
		}
	})
	eventually(t, func() bool {
		select {
		case err := <-done:
			t.Fatal("host exited before external evidence", err)
		default:
		}
		_, err := os.Stat(evidence)
		return err == nil
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("external child did not exit on EOF/cancellation")
	}
}

func TestExternalPluginRunRejectsChangedFileBeforeExec(t *testing.T) {
	store, session := hostStore(t)
	file := filepath.Join(t.TempDir(), "plugin")
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	body := "#!/bin/sh\n: > '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(file, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "external")
	if err := feed.AddExternalPlugin(file, nil); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginFeed([]string{session.ID}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body+"# changed\n"), 0700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), store, binary, "external", io.Discard)
	if model.ErrorCodeOf(err) != model.CodeConflict || !strings.Contains(err.Error(), "launch changed") {
		t.Fatal("changed executable reached child startup", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("changed executable ran", err)
	}
}

type externalObservationCheck struct {
	store                                          bus.Store
	binary, evidence, marker, data, home, sentinel string
	literals                                       []string
}

func (check *externalObservationCheck) run(t *testing.T) {
	store, binary, evidence, marker, data, home, sentinel, literals := check.store, check.binary, check.evidence, check.marker, check.data, check.home, check.sentinel, check.literals
	runExternalUntilObserved(t, store, binary, evidence)
	var observed externalEvidence
	raw, err := os.ReadFile(evidence)
	if err != nil || json.Unmarshal(raw, &observed) != nil {
		t.Fatal("missing external evidence", err)
	}
	if !slices.Equal(observed.Args, literals) {
		t.Fatal("literal argv changed or shell was used", observed.Args)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("shell substitution executed", err)
	}
	expected, err := filepath.EvalSymlinks(data)
	if err != nil || observed.Cwd != expected {
		t.Fatal("external cwd was not persistent data", observed.Cwd, expected, err)
	}
	if observed.Home != home || observed.EnvironmentCount != 1 || observed.SentinelInherited {
		t.Fatal("external child inherited environment", observed.EnvironmentCount, observed.SentinelInherited)
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "keep" {
		t.Fatal("restart removed persistent data", err)
	}
	if err := os.Remove(evidence); err != nil {
		t.Fatal(err)
	}
}
