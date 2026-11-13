package bus

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestSessionPluginFilesystemCrashCuts(t *testing.T) {
	if root := os.Getenv("ZALKI_Z6_FS_ROOT"); root != "" {
		runSessionPluginCrash(t, root)
		return
	}
	for _, muted := range []bool{true, false} {
		for cut := range 3 {
			t.Run(strconv.FormatBool(muted)+strconv.Itoa(cut), func(t *testing.T) { assertSessionPluginCrash(t, muted, cut) })
		}
	}
}

func assertSessionPluginCrash(t *testing.T, muted bool, cut int) {
	t.Helper()
	s := fixtureStore(t)
	v, key := register(t, s, "cursor")
	if !muted {
		if err := SetSessionPlugin(s, v.ID, key, "external", true); err != nil {
			t.Fatal(err)
		}
	}
	_, old := pluginPolicyForTest(t, s, v.ID)
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSessionPluginFilesystemCrashCuts$", "-test.count=1")
	child.Env = append(os.Environ(), "ZALKI_Z6_FS_ROOT="+s.Dir, "ZALKI_Z6_FS_ACTOR="+v.ID, "ZALKI_Z6_FS_KEY="+key, "ZALKI_Z6_FS_MUTED="+strconv.FormatBool(muted), "ZALKI_Z6_FS_CUT="+strconv.Itoa(cut))
	err := child.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatal("Z6_ACTUAL_POLICY_CRASH_REACHED", err)
	}
	if err := s.locked(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, got := pluginPolicyForTest(t, s, v.ID)
	expected, err := os.ReadFile(filepath.Join(s.Dir, "expected-policy-audit"))
	if err != nil {
		t.Fatal(err)
	}
	_, isMuted := got.Muted["external"]
	if cut == 0 {
		if got.LastChange.ID != old.LastChange.ID || isMuted == muted {
			t.Fatal("Z6_CRASH_PRE_RENAME_ATOMIC_OLD_POLICY")
		}
	} else if got.LastChange.ID != string(expected) || isMuted != muted {
		t.Fatal("Z6_CRASH_POST_RENAME_ATOMIC_NEW_POLICY")
	}
	id := got.LastChange.ID
	if err := SetSessionPlugin(s, v.ID, key, "external", muted); err != nil {
		t.Fatal(err)
	}
	_, retry := pluginPolicyForTest(t, s, v.ID)
	if cut > 0 && retry.LastChange.ID != id {
		t.Fatal("Z6_CRASH_PUBLISHED_RETRY_NO_NEW_AUDIT")
	}
}

func runSessionPluginCrash(t *testing.T, root string) {
	t.Helper()
	s := Store{Dir: root, Registry: testRegistry()}
	actor := os.Getenv("ZALKI_Z6_FS_ACTOR")
	key := os.Getenv("ZALKI_Z6_FS_KEY")
	muted, err := strconv.ParseBool(os.Getenv("ZALKI_Z6_FS_MUTED"))
	if err != nil {
		t.Fatal(err)
	}
	cut, err := strconv.Atoi(os.Getenv("ZALKI_Z6_FS_CUT"))
	if err != nil {
		t.Fatal(err)
	}
	writer := sessionPluginWriter{sync: journal.SyncDir, save: func(s Store, next model.Session) error {
		var policy sessionPluginPolicy
		if err := DecodeStrictRecordJSON(next.PluginPolicy, &policy); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, "expected-policy-audit"), []byte(policy.LastChange.ID), 0600); err != nil {
			return err
		}
		path, err := s.sessionPath(actor)
		if err != nil {
			return err
		}
		raw, err := encodeStateJSON(next)
		if err != nil {
			return err
		}
		temp := filepath.Join(filepath.Dir(path), ".z6-policy-crash-image")
		if err := journal.WriteAtomic(temp, raw); err != nil {
			return err
		}
		if cut == 0 {
			os.Exit(23)
		}
		if err := os.Rename(temp, path); err != nil {
			return err
		}
		if cut == 1 {
			os.Exit(23)
		}
		if err := journal.SyncDir(filepath.Dir(path)); err != nil {
			return err
		}
		os.Exit(23)
		return nil
	}}
	err = setSessionPlugin(s, actor, key, "external", muted, writer)
	t.Fatal("Z6_POLICY_CRASH_POINT_NOT_REACHED", err)
}
