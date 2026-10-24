package bus

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestInstallationPlanNeverWritesEvenWhenApplyRequested(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	request := InstallationRequest{Action: "install", Binary: "/old/zalki", StateDir: state,
		HomeDir: home, Apply: true}
	before := installationSnapshot(t, home)
	plan, err := defaultRegistry.planInstallation(request)
	if err != nil || len(plan.edits) == 0 {
		t.Fatal("valid installation produced no plan", err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("planning created settings, backups, state, or lock files")
	}
	if _, err := ManageInstallation(request); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"upgrade", "uninstall"} {
		request.Action, request.Binary = action, "/new/zalki"
		before := installationSnapshot(t, home)
		plan, err := defaultRegistry.planInstallation(request)
		if err != nil || len(plan.edits) == 0 {
			t.Fatalf("%s produced no plan: %v", action, err)
		}
		if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
			t.Fatalf("%s planning changed installation files", action)
		}
	}
}
