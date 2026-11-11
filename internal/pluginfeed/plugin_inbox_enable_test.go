package pluginfeed

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginInboxOwnerRepairsHintsWithoutTrustingThem(t *testing.T) {
	for _, kind := range []string{"valid", "null", "missing", "wrong-name", "relative-name", "wrong-type", "oversize", "wrong-id"} {
		t.Run(kind, func(t *testing.T) { checkOwnerInboxHint(t, kind) })
	}
}
func checkOwnerInboxHint(t *testing.T, kind string) {
	t.Helper()
	x := newInboxStorageFixture(t)
	id := x.status.ID
	hint := filepath.Join(x.f.pluginDir(), "id-"+id+".json")
	before, err := os.Stat(hint)
	if err != nil {
		t.Fatal(err)
	}
	recordBefore, _ := os.ReadFile(x.f.pluginPath())
	request := x.request()
	poisonOwnerInboxHint(t, x, kind, hint, &request)
	if kind != "valid" {
		m, err := x.service.Send(context.Background(), request)
		assertInboxFailure(t, m, err)
		if err.Error() != "unauthorized: plugin inbox unavailable" {
			t.Fatal("bad hint leaked detail", err)
		}
	}
	if err = x.f.EnablePluginFeed(true); err != nil {
		t.Fatal("explicit owner hint repair failed", kind, err)
	}
	after, err := os.Stat(hint)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "valid" && !os.SameFile(before, after) {
		t.Fatal("valid hint was rewritten")
	}
	raw, err := os.ReadFile(hint)
	want, _ := journal.EncodeJSON(x.f.name, 128)
	if err != nil || !bytes.Equal(raw, want) {
		t.Fatal("owner did not repair exact hint", err)
	}
	recordAfter, _ := os.ReadFile(x.f.pluginPath())
	if !bytes.Equal(recordBefore, recordAfter) {
		t.Fatal("hint repair changed live installation or feed fence")
	}
	if kind == "wrong-id" {
		m, err := x.service.Send(context.Background(), request)
		assertInboxFailure(t, m, err)
		return
	}
	request = x.request()
	if _, err = x.service.Send(context.Background(), request); err != nil {
		t.Fatal("repaired hint did not route", err)
	}
}
func poisonOwnerInboxHint(t *testing.T, x inboxStorageFixture, kind, hint string, r *bus.SendRequest) {
	t.Helper()
	var raw []byte
	switch kind {
	case "valid":
		return
	case "missing":
		if err := os.Remove(hint); err != nil {
			t.Fatal(err)
		}
		return
	case "null":
		raw = []byte(`null`)
	case "wrong-name":
		raw = []byte(`"another-plugin"`)
	case "relative-name":
		raw = []byte(`"../x"`)
	case "wrong-type":
		raw = []byte(`123`)
	case "oversize":
		raw = []byte(strings.Repeat("x", 129))
	case "wrong-id":
		id := model.NewUUID()
		hint = filepath.Join(x.f.pluginDir(), "id-"+id+".json")
		r.To = "plugin:" + id
		raw, _ = journal.EncodeJSON(x.f.name, 128)
	}
	if err := os.WriteFile(hint, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPluginInboxValidHintKeepsEnablePublicationOrder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory sync cut requires ordinary user")
	}
	x := newInboxStorageFixture(t)
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	hint := filepath.Join(x.f.pluginDir(), "id-"+x.status.ID+".json")
	before, err := os.Stat(hint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(x.f.pluginDir(), 0700) })
	if err = os.Chmod(x.f.pluginDir(), 0300); err != nil {
		t.Fatal(err)
	}
	err = x.f.EnablePluginFeed(true)
	if restoreErr := os.Chmod(x.f.pluginDir(), 0700); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal("cut did not fail at directory sync", err)
	}
	p, _, err := x.f.loadPlugin()
	if err != nil || !p.Enabled {
		t.Fatal("valid hint sync preceded legacy record publication", err)
	}
	after, err := os.Stat(hint)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("valid hint was replaced before enable commit", err)
	}
}
