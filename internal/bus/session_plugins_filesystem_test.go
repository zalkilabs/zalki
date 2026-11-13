package bus

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestSessionPluginRealFilesystemFailureCuts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission failure requires an unprivileged owner")
	}
	for _, cut := range []string{"pre-rename", "post-rename"} {
		t.Run(cut, func(t *testing.T) { assertSessionPluginFilesystemCut(t, cut) })
	}
}

func assertSessionPluginFilesystemCut(t *testing.T, cut string) {
	t.Helper()
	s := fixtureStore(t)
	v, key := register(t, s, "cursor")
	path, _ := s.sessionPath(v.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := filesystemSessionPolicyWriter(t, path, cut)
	if err := setSessionPlugin(s, v.ID, key, "external", true, writer); err == nil || !strings.HasSuffix(err.Error(), pluginChangeUncertain) {
		t.Fatal("Z6_REAL_FS_ERROR_NEVER_SUCCESS", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) != (cut == "pre-rename") {
		t.Fatal("Z6_PRE_RENAME_UNCHANGED_POST_RENAME_PUBLISHED", cut)
	}
	if cut == "post-rename" {
		_, policy := pluginPolicyForTest(t, s, v.ID)
		id := policy.LastChange.ID
		if sessionAllowsPlugin(v, "external") == false {
			t.Fatal("fixture initial policy")
		}
		if pluginMemberForTest(t, s, v.ID, "external") {
			t.Fatal("Z6_PUBLISHED_FS_POLICY_IS_CURRENT_TRUTH")
		}
		if err := SetSessionPlugin(s, v.ID, key, "external", true); err != nil {
			t.Fatal(err)
		}
		_, retry := pluginPolicyForTest(t, s, v.ID)
		if retry.LastChange.ID != id {
			t.Fatal("Z6_PUBLISHED_FS_RETRY_RETAINS_ID")
		}
	}
}

func TestSessionPluginPrepublicationFailureHasNoRollback(t *testing.T) {
	s := fixtureStore(t)
	v, key := register(t, s, "cursor")
	path, _ := s.sessionPath(v.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := sessionPluginWriter{sync: journal.SyncDir, save: func(Store, model.Session) error { return errors.New("failed before writing") }}
	if err := setSessionPlugin(s, v.ID, key, "external", true, writer); err == nil {
		t.Fatal("Z6_PREPUBLICATION_FAILURE_REPORTED")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("Z6_UNPUBLISHED_FAILURE_PRESERVES_BYTES", err)
	}
}

func filesystemSessionPolicyWriter(t *testing.T, path, cut string) sessionPluginWriter {
	t.Helper()
	return sessionPluginWriter{sync: journal.SyncDir, save: func(s Store, next model.Session) error {
		dir := filepath.Dir(path)
		if cut == "pre-rename" {
			if err := os.Chmod(dir, 0500); err != nil {
				return err
			}
			defer restorePolicyDirectory(t, dir)
			return saveSessionFor(s, next, "")
		}
		raw, err := encodeStateJSON(next)
		if err != nil {
			return err
		}
		temp := filepath.Join(dir, ".test-policy-after-image")
		if err := journal.WriteAtomic(temp, raw); err != nil {
			return err
		}
		if err := os.Rename(temp, path); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0000); err != nil {
			return err
		}
		defer restorePolicyDirectory(t, dir)
		return journal.SyncDir(dir)
	}}
}

func restorePolicyDirectory(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Error(err)
	}
}
