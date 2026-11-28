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
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/statefile"
)

type externalEvidence struct {
	Args              []string
	Cwd               string
	Home              string
	EnvironmentCount  int
	SentinelInherited bool
}

// noteHostFileInCwd leaves a marker when the working directory holds a file
// named inbox.json: a Zalki file the child must never see there.
func noteHostFileInCwd(cwd string) error {
	if _, err := os.Stat("inbox.json"); err == nil {
		return os.WriteFile(filepath.Join(cwd, "saw-inbox-json"), nil, 0600)
	}
	return nil
}

func externalTestChild(args []string) error {
	if err := checkFixtureLeaseFD3(); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := noteHostFileInCwd(cwd); err != nil {
		return err
	}
	e := externalEvidence{args[1:], cwd, os.Getenv("HOME"), len(os.Environ()), os.Getenv("ZALKI_EXTERNAL_SENTINEL") != ""}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(args[0], raw); err != nil {
		return err
	}
	if err := writeFrame(os.Stdout, frame{Hello: &hello{Wire: wireName, Requires: []string{"feed"}}}); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(os.Stdin, maxFrame+1)
	for {
		if skipHostFrame(reader) {
			continue // the host's hello and hello_ok
		}
		value, err := readFrame(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		raw, err := statefile.EncodeJSON(value, maxFrame)
		if err != nil {
			return err
		}
		if err := fsutil.WriteAtomic(args[0]+".last", raw); err != nil {
			return err
		}
		if err := writeFrame(os.Stdout, frame{Ack: value.ID}); err != nil {
			return err
		}
	}
}

func TestExternalPluginLiteralArgsHomeOnlyAndPersistentCwd(t *testing.T) {
	store, _ := hostStore(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "external")
	data := filepath.Join(store.Dir(), "plugins", "external.data")
	evidence := filepath.Join(data, "observed.json")
	marker := filepath.Join(data, "shell-was-used")
	literals := []string{"$(/usr/bin/touch " + marker + ")", "$HOME", "literal space", "--not-an-internal-flag"}
	args := append([]string{"external-test-child", evidence}, literals...)
	if err := feed.AddExternalPlugin(binary, args); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.GrantPluginAccess(pluginfeed.PluginAccess{Status: true}); err != nil {
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
	check := externalObservationCheck{store, evidence, marker, data, home, sentinel, literals}
	t.Run("initial", check.run)
	t.Run("restart", check.run)
}

func runExternalUntilObserved(t *testing.T, store bus.Store, evidence string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, store, "external", io.Discard, nil); close(done) }()
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
	store, _ := hostStore(t)
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
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.GrantPluginAccess(pluginfeed.PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body+"# changed\n"), 0700); err != nil {
		t.Fatal(err)
	}
	err := Run(t.Context(), store, "external", io.Discard, nil)
	if model.ErrorCodeOf(err) != model.CodeConflict || !strings.Contains(err.Error(), "launch changed") {
		t.Fatal("changed executable reached child startup", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("changed executable ran", err)
	}
}

type externalObservationCheck struct {
	store                                  bus.Store
	evidence, marker, data, home, sentinel string
	literals                               []string
}

func (check *externalObservationCheck) run(t *testing.T) {
	store, evidence, marker, data, home, sentinel, literals := check.store, check.evidence, check.marker, check.data, check.home, check.sentinel, check.literals
	runExternalUntilObserved(t, store, evidence)
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
	if observed.Home != home || observed.EnvironmentCount != 2 || observed.SentinelInherited {
		t.Fatal("external child inherited environment", observed.EnvironmentCount, observed.SentinelInherited)
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "keep" {
		t.Fatal("restart removed persistent data", err)
	}
	if err := os.Remove(evidence); err != nil {
		t.Fatal(err)
	}
}

// The child must never see a Zalki file in its working directory: an index
// that an earlier build kept there is moved out before the child starts.
func TestExternalPluginRunMovesAnEarlierIndexBeforeTheChildStarts(t *testing.T) {
	store, _ := hostStore(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "external")
	data := filepath.Join(store.Dir(), "plugins", "external.data")
	evidence := filepath.Join(data, "observed.json")
	if err := feed.AddExternalPlugin(binary, []string{"external-test-child", evidence}); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.GrantPluginAccess(pluginfeed.PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := statefile.EncodeJSON(map[string]any{
		"version": 1, "installation": status.ID, "entries": []any{}, "quota": model.NewFloodState(time.Now().UTC()),
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteAtomic(filepath.Join(data, "inbox.json"), raw); err != nil {
		t.Fatal(err)
	}
	runExternalUntilObserved(t, store, evidence)
	if _, err := os.Stat(filepath.Join(data, "saw-inbox-json")); !os.IsNotExist(err) {
		t.Fatal("the child saw the host's index file in its working directory", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), "plugins", "external.inbox.json")); err != nil {
		t.Fatal("the index was not moved next to the other plugin records", err)
	}
	if _, err := os.Stat(filepath.Join(data, "inbox.json")); !os.IsNotExist(err) {
		t.Fatal("the earlier file is still in the data folder", err)
	}
}

// An earlier index that cannot be moved must not leave a started plugin beside it.
func TestExternalPluginRunDoesNotStartWhenAnEarlierIndexCannotBeMoved(t *testing.T) {
	store, _ := hostStore(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "external")
	plugins := filepath.Join(store.Dir(), "plugins")
	data := filepath.Join(plugins, "external.data")
	evidence := filepath.Join(data, "observed.json")
	if err := feed.AddExternalPlugin(binary, []string{"external-test-child", evidence}); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.GrantPluginAccess(pluginfeed.PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	runExternalUntilObserved(t, store, evidence) // creates the lease file and the new index
	status, err := feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := statefile.EncodeJSON(map[string]any{
		"version": 1, "installation": status.ID, "entries": []any{}, "quota": model.NewFloodState(time.Now().UTC()),
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteAtomic(filepath.Join(data, "inbox.json"), raw); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(plugins, "external.inbox.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(evidence); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plugins, 0500); err != nil { // the move cannot write the new file
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(plugins, 0700) })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := Run(ctx, store, "external", io.Discard, nil); err == nil {
		t.Fatal("the plugin ran although its index could not be moved")
	}
	if _, err := os.Stat(evidence); !os.IsNotExist(err) {
		t.Fatal("the child started", err)
	}
}

// A file named inbox.json that is not a Zalki list is the plugin's data: the
// plugin starts and the file stays as it is.
func TestExternalPluginRunStartsAndLeavesAFileThatIsNotOurList(t *testing.T) {
	store, _ := hostStore(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "external")
	data := filepath.Join(store.Dir(), "plugins", "external.data")
	evidence := filepath.Join(data, "observed.json")
	if err := feed.AddExternalPlugin(binary, []string{"external-test-child", evidence}); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.GrantPluginAccess(pluginfeed.PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(data, "inbox.json")
	if err := os.WriteFile(own, []byte("the plugin's own data"), 0600); err != nil {
		t.Fatal(err)
	}
	runExternalUntilObserved(t, store, evidence)
	if got, err := os.ReadFile(own); err != nil || string(got) != "the plugin's own data" {
		t.Fatalf("the plugin's file was changed: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), "plugins", "external.inbox.json")); err != nil {
		t.Fatal("the list was not created", err)
	}
}
