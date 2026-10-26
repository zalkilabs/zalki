package install_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

func TestInstallationPlanNeverWritesEvenWhenApplyRequested(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	request := install.Request{Action: "install", Binary: "/old/zalki", StateDir: state,
		HomeDir: home, Apply: true}
	before := installationSnapshot(t, home)
	edits, err := install.PlannedEdits(declare, request)
	if err != nil || edits == 0 {
		t.Fatal("valid installation produced no plan", err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("planning created settings, backups, state, or lock files")
	}
	if _, err := install.Manage(declare, request); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"upgrade", "uninstall"} {
		request.Action, request.Binary = action, "/new/zalki"
		before := installationSnapshot(t, home)
		edits, err := install.PlannedEdits(declare, request)
		if err != nil || edits == 0 {
			t.Fatalf("%s produced no plan: %v", action, err)
		}
		if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
			t.Fatalf("%s planning changed installation files", action)
		}
	}
}
