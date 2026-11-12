package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestNativeIdentityIsIndependentOfJournalAddress(t *testing.T) {
	store := fixtureStore(t)
	registry, err := agent.NewRegistry(fixtureAdapter{kind: "fourth", inbox: model.InboxNative})
	if err != nil {
		t.Fatal(err)
	}
	store.Registry = registry
	native := "workspace/branch:conversation-42"
	first, key, err := store.Register("fourth", native, "/workspace", nil)
	if err != nil || first.NativeID != native {
		t.Fatal(first, err)
	}
	path, err := store.sessionPath(first.ID)
	if err != nil || strings.Contains(path, "conversation") {
		t.Fatal("native ID became a journal path", path, err)
	}
	if _, err := model.UUID(strings.TrimPrefix(first.ID, "fourth:")); err != nil {
		t.Fatal(err)
	}
	second, _, err := store.Register("fourth", native, "/workspace", nil)
	if err != nil || second.ID != first.ID || second.Generation == first.Generation {
		t.Fatal("registration lost its stable address or generation fence", err)
	}
	if _, err := store.authenticate(first.ID, key); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("old capability survived registration", err)
	}
	from, fromKey, err := store.Register("fourth", "another-native-id", "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Deliver: func(_ context.Context, _ model.Message, target model.Session) model.Receipt {
		if target.NativeID != native || target.ID != first.ID || target.KeyHash != "" {
			t.Error("adapter did not receive the original native identity with credentials removed")
		}
		return model.Receipt{Status: model.StatusSubmitted}
	}}
	if _, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: fromKey, To: first.ID, MessageID: newID(), Body: "ping"}); err != nil {
		t.Fatal(err)
	}
	// A UUID-shaped native ID cannot overwrite a mapped non-UUID identity.
	if _, _, err := store.Register("fourth", strings.TrimPrefix(first.ID, "fourth:"), "/workspace", nil); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("native identity collision accepted", err)
	}
	input, _ := json.Marshal(agent.HookInput{SessionID: native, CWD: "/workspace", Event: "end"})
	if err := service.Hook(t.Context(), "fourth", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	current, err := store.session(first.ID)
	if err != nil || !current.Retired {
		t.Fatal("native hook did not resolve the mapped address", err)
	}
}

func TestNativeIdentityValidationAndUUIDCompatibility(t *testing.T) {
	store := fixtureStore(t)
	id := newID()
	v, _, err := store.Register("codex", strings.ToUpper(id), "/workspace", nil)
	if err != nil || v.ID != "codex:"+id || v.NativeID != id {
		t.Fatal("legacy UUID address changed", err)
	}
	for _, invalid := range []string{"", "id\nforged", "id\x00", string([]byte{255}), strings.Repeat("a", 1025)} {
		if _, _, err := store.Register("codex", invalid, "/workspace", nil); model.ErrorCodeOf(err) != model.CodeInvalidIdentity {
			t.Fatal("invalid native ID accepted", err)
		}
	}
}

func TestOpaqueAdapterStateSurvivesCompaction(t *testing.T) {
	store := fixtureStore(t)
	registry, err := agent.NewRegistry(fixtureAdapter{kind: "fourth", inbox: model.InboxNative})
	if err != nil {
		t.Fatal(err)
	}
	store.Registry = registry
	v, oldKey, err := store.Register("fourth", "native-conversation", "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	v.AdapterState = json.RawMessage(`{"schema":1,"client_field":"keep"}`)
	v.State = model.StateBusy
	if err := store.saveSession(v); err != nil {
		t.Fatal(err)
	}
	next, key, err := store.register(v.Kind, v.NativeID, v.CWD, nil, true)
	if err != nil || next.Generation != v.Generation || next.State != v.State || !equalJSON(next.AdapterState, v.AdapterState) {
		t.Fatal("compaction lost adapter-owned state", err)
	}
	if _, err := store.authenticate(v.ID, oldKey); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("compaction retained the old credential", err)
	}
	if _, err := store.authenticate(v.ID, key); err != nil {
		t.Fatal("compaction did not install the new credential", err)
	}
	if err := store.Update(v.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	replacement, _, err := store.register(v.Kind, v.NativeID, v.CWD, nil, true)
	if err != nil || replacement.Generation == next.Generation || replacement.Retired {
		t.Fatal("compaction reused a retired generation", err)
	}
}

func TestFutureRecordsAreNeverOverwritten(t *testing.T) {
	for _, kind := range []string{"session", "message", "adapter"} {
		t.Run(kind, func(t *testing.T) {
			store := fixtureStore(t)
			v, key := register(t, store, "cursor")
			path, _ := store.sessionPath(v.ID)
			msg := model.Message{ID: newID(), From: "codex:" + newID(), To: v.ID, Body: "ping", Status: model.StatusSubmitted}
			if kind == "message" {
				path = filepath.Join(store.Dir, "messages", msg.ID+".json")
				msg.Schema = 99
				if err := writeJSON(path, msg); err != nil {
					t.Fatal(err)
				}
			} else {
				if kind == "session" {
					v.Schema = 99
				} else {
					v.AdapterState = json.RawMessage(`{"schema":99}`)
				}
				if err := writeJSON(path, v); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			var err error
			want := model.CodeUnsupportedSchema
			if kind == "message" {
				_, err = store.Ack(v.ID, key, msg.ID)
				want = model.CodeNotFound
			} else {
				err = store.Update(v.ID, model.StateBusy, false)
			}
			if model.ErrorCodeOf(err) != want {
				t.Fatal("future record did not get the expected refusal", err)
			}
			if kind == "message" {
				requireFutureMessageSkipped(t, store, v.ID, key)
			} else {
				_, err = store.List()
			}
			if kind != "message" && err != nil {
				t.Fatal("session scan did not skip unsupported record", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("future record changed")
			}
		})
	}
}

// requireFutureMessageSkipped checks that the list skips and counts a future
// message record, like the session scan; exact reads (Ack) stay strict.
func requireFutureMessageSkipped(t *testing.T, store Store, actor, key string) {
	t.Helper()
	batch, err := store.ReadMessages(MessageQuery{Actor: actor, Capability: key, History: true})
	if err != nil || batch.Unreadable != 1 {
		t.Fatal("future message record was not skipped and counted", batch.Unreadable, err)
	}
}

// A session record with another schema (2, the old lease schema) is damaged for
// this build. Every registry-wide read skips it, and nothing rewrites it.
func TestFutureSchemaSessionSkipsRegistryReadsWithoutWriting(t *testing.T) {
	store := fixtureStore(t)
	from, key, healthy, _ := registerPair(t, store, "codex", "claude")
	future, _ := register(t, store, "cursor")
	path, _ := store.sessionPath(future.ID)
	var record map[string]any
	if err := readJSON(path, &record); err != nil {
		t.Fatal(err)
	}
	record["schema"] = 2
	record["future_lease"] = map[string]string{"id": newID()}
	if err := writeJSON(path, record); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if rows, skipped, err := store.ListWithWarnings(); err != nil || len(rows) != 2 || skipped != 1 {
		t.Fatal("list lost healthy sessions or skip count", rows, skipped, err)
	}
	if rows, skipped, err := store.DiscoverWithWarnings("", ""); err != nil || len(rows) != 2 || skipped != 1 {
		t.Fatal("discovery lost healthy sessions or skip count", rows, skipped, err)
	}
	if report, err := store.Diagnostics(t.Context()); err != nil || len(report.Sessions) != 2 {
		t.Fatal("doctor lost healthy sessions", report, err)
	}
	if err := store.Update(future.ID, model.StateBusy, false); model.ErrorCodeOf(err) != model.CodeUnsupportedSchema {
		t.Fatal("lifecycle update accepted a future session record", err)
	}
	if _, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: future.ID, MessageID: newID(), Body: "body"}); model.ErrorCodeOf(err) != model.CodeUnsupportedSchema {
		t.Fatal("send accepted a future recipient record", err)
	}
	if _, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: healthy.ID, MessageID: newID(), Body: "body"}); err != nil {
		t.Fatal("exact-ID send to a readable session failed", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("a failed read or update rewrote the future session record")
	}
}

func TestInterruptedLeaseIsRetiredByRecovery(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	lease := armedLease(t, store, to.ID, key)
	// A crash releases the lifetime lock but leaves the lease record behind.
	if err := syscall.Flock(lease.fd, syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if grant, err := claimCursorRecovery(store, to.ID); err != nil || grant.Capability == "" {
		t.Fatal("an interrupted lease blocked recovery", err)
	}
	if current, err := store.session(to.ID); err != nil || current.Listener != nil {
		t.Fatalf("recovery did not retire the interrupted lease: %+v %v", current, err)
	}
}
