package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type reservationOrderAdapter struct {
	stubCodexAdapter
	kind  string
	live  *model.Liveness
	calls *[]string
}

func (a reservationOrderAdapter) Descriptor() agent.AdapterDescriptor {
	*a.calls = append(*a.calls, a.kind+".descriptor")
	d := a.stubCodexAdapter.Descriptor()
	d.Kind, d.Label = a.kind, a.kind
	return d
}

func (a reservationOrderAdapter) Liveness(agent.LivenessView, agent.LivenessProbes) model.Liveness {
	*a.calls = append(*a.calls, a.kind+".liveness")
	return *a.live
}

func (reservationOrderAdapter) TracksOwnProcess() bool { return false }

func (reservationOrderAdapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	panic("reservation must not invoke transport")
}

type reservationOrderFixture struct {
	store          Store
	sender, target model.Session
	request        SendRequest
	live           *model.Liveness
	calls          *[]string
	log            bytes.Buffer
	logReads       int
	logReadErr     error
	loggedMessage  model.Message
}

func newReservationOrderFixture(t *testing.T) *reservationOrderFixture {
	t.Helper()
	var calls []string
	live := model.LivenessAlive
	registry, err := agent.NewRegistry(
		reservationOrderAdapter{kind: "alpha", live: &live, calls: &calls},
		reservationOrderAdapter{kind: "beta", live: &live, calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: registry}
	sender, key, target, _ := registerPair(t, store, "alpha", "beta")
	f := &reservationOrderFixture{store: store, sender: sender, target: target, live: &live, calls: &calls}
	f.request = SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "characterized payload"}
	f.store.Logger = slog.New(slog.NewJSONHandler(&f.log, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.MessageKey && attr.Value.String() == "message.reserved" {
				f.logReads++
				raw, readErr := os.ReadFile(filepath.Join(f.store.Dir, "messages", f.request.MessageID+".json"))
				f.logReadErr = readErr
				if readErr == nil {
					f.logReadErr = json.Unmarshal(raw, &f.loggedMessage)
				}
			}
			return attr
		},
	}))
	return f
}

func reserveCharacterizedMessage(f *reservationOrderFixture, persist bool) (message model.Message, target model.Session, fresh bool, err error) {
	*f.calls = nil
	f.log.Reset()
	err = f.store.locked(func() error {
		var reserveErr error
		message, target, fresh, reserveErr = f.store.reserveNewMessage(f.sender, f.request, persist)
		return reserveErr
	})
	return
}

func saveReservationTarget(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	if err := f.store.saveSession(f.target); err != nil {
		t.Fatal(err)
	}
}

func setFullReservationApproval(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	f.target.State, f.target.ApprovalHeld = model.StateApproval, approvalQueueCap
	saveReservationTarget(t, f)
}

func seedInvalidReservationQuota(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	path, err := f.store.floodPath(f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":99}`), 0600); err != nil {
		t.Fatal(err)
	}
}

func seedEmptyReservationQuota(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	path, err := f.store.floodPath(f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	state := withFloodQuota(floodRecord{Schema: 1, Target: f.target.ID}, model.NewFloodState(future))
	state.Senders[derivedID("zalki/flood/sender/v1", f.request.Actor)] = model.FloodBucket{At: future}
	if err := f.store.saveFloodState(path, state); err != nil {
		t.Fatal(err)
	}
}

func seedReservationReply(t *testing.T, f *reservationOrderFixture, wrongTo bool) {
	t.Helper()
	parent := model.Message{ID: newID(), From: f.target.ID, To: f.request.Actor, Body: "parent", Status: model.StatusSubmitted}
	if wrongTo {
		parent.To = f.target.ID
	} else {
		parent.From = f.request.Actor
	}
	if err := f.store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	f.request.ReplyTo = parent.ID
}

func reservationSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err == nil {
			files[rel] = string(raw)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
