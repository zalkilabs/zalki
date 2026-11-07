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
	if _, err := UUID(strings.TrimPrefix(first.ID, "fourth:")); err != nil {
		t.Fatal(err)
	}
	second, _, err := store.Register("fourth", native, "/workspace", nil)
	if err != nil || second.ID != first.ID || second.Generation == first.Generation {
		t.Fatal("registration lost its stable address or generation fence", err)
	}
	if _, err := store.authenticate(first.ID, key); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("old capability survived registration", err)
	}
	from, fromKey, err := store.Register("fourth", "another-native-id", "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Deliver: func(_ context.Context, _ Message, target Session) Receipt {
		if NativeID(target) != native || target.ID != first.ID || target.KeyHash != "" {
			t.Error("adapter did not receive the original native identity with credentials removed")
		}
		return Receipt{Status: StatusSubmitted}
	}}
	if _, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: fromKey, To: first.ID, MessageID: newID(), Body: "ping"}); err != nil {
		t.Fatal(err)
	}
	// A UUID-shaped native ID cannot overwrite a mapped non-UUID identity.
	if _, _, err := store.Register("fourth", strings.TrimPrefix(first.ID, "fourth:"), "/workspace", nil); ErrorCodeOf(err) != CodeConflict {
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
		if _, _, err := store.Register("codex", invalid, "/workspace", nil); ErrorCodeOf(err) != CodeInvalidIdentity {
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
	v.State = StateBusy
	if err := store.saveSession(v); err != nil {
		t.Fatal(err)
	}
	next, key, err := store.register(v.Kind, v.NativeID, v.CWD, nil, true)
	if err != nil || next.Generation != v.Generation || next.State != v.State || !equalJSON(next.AdapterState, v.AdapterState) {
		t.Fatal("compaction lost adapter-owned state", err)
	}
	if _, err := store.authenticate(v.ID, oldKey); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("compaction retained the old credential", err)
	}
	if _, err := store.authenticate(v.ID, key); err != nil {
		t.Fatal("compaction did not install the new credential", err)
	}
	if err := store.Update(v.ID, StateRetired, true); err != nil {
		t.Fatal(err)
	}
	replacement, _, err := store.register(v.Kind, v.NativeID, v.CWD, nil, true)
	if err != nil || replacement.Generation == next.Generation || replacement.Retired {
		t.Fatal("compaction reused a retired generation", err)
	}
}

func TestLegacyRecordsMigrateOnWrite(t *testing.T) {
	store := fixtureStore(t)
	v, key := register(t, store, "cursor")
	path, _ := store.sessionPath(v.ID)
	var legacy map[string]any
	if err := readJSON(path, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "schema")
	delete(legacy, "native_id")
	delete(legacy, "adapter_state")
	legacy["cursor_wake"] = model.Endpoint{Path: "/fixture/wake"}
	legacy["cursor_recovery_attempted"], legacy["cursor_recovery_suppressed"] = true, true
	if err := writeJSON(path, legacy); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	loaded, err := store.session(v.ID)
	state := cursorState(loaded)
	// A pre-lease wake endpoint is dropped, never adopted as a lease.
	if err != nil || NativeID(loaded) != NativeID(v) || !state.RecoveryAttempted || !state.RecoverySuppressed || loaded.Listener != nil || loaded.Receive.IdleWake {
		t.Fatal("legacy Cursor recovery state lost or legacy wake adopted", err)
	}
	if _, err := store.List(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("discovery rewrote a legacy record")
	}
	if err := store.Update(v.ID, StateBusy, false); err != nil {
		t.Fatal(err)
	}
	var upgraded map[string]any
	if err := readJSON(path, &upgraded); err != nil {
		t.Fatal(err)
	}
	if upgraded["schema"] != float64(recordSchema) || upgraded["native_id"] != NativeID(v) || upgraded["adapter_state"] == nil || upgraded["cursor_wake"] != nil || upgraded["cursor_recovery_attempted"] != nil || upgraded["listener"] != nil {
		t.Fatal("adapter state was not migrated", upgraded)
	}
	from, _ := register(t, store, "codex")
	msg := Message{ID: newID(), From: from.ID, To: v.ID, Body: "ping", Status: StatusSubmitted}
	messagePath := filepath.Join(store.Dir, "messages", msg.ID+".json")
	if err := writeJSON(messagePath, msg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ack(v.ID, key, msg.ID); err != nil {
		t.Fatal(err)
	}
	var migrated Message
	if err := readJSON(messagePath, &migrated); err != nil || migrated.Schema != recordSchema || migrated.AcknowledgedAt == "" {
		t.Fatal("message migration lost ACK", err)
	}
}

func TestFutureRecordsAreNeverOverwritten(t *testing.T) {
	for _, kind := range []string{"session", "message", "adapter"} {
		t.Run(kind, func(t *testing.T) {
			store := fixtureStore(t)
			v, key := register(t, store, "cursor")
			path, _ := store.sessionPath(v.ID)
			msg := Message{ID: newID(), From: "codex:" + newID(), To: v.ID, Body: "ping", Status: StatusSubmitted}
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
			if kind == "message" {
				_, err = store.Ack(v.ID, key, msg.ID)
			} else {
				err = store.Update(v.ID, StateBusy, false)
			}
			if ErrorCodeOf(err) != CodeUnsupportedSchema {
				t.Fatal("future schema was accepted", err)
			}
			if kind == "message" {
				_, err = store.Messages(v.ID, key)
			} else {
				_, err = store.List()
			}
			if kind == "message" && ErrorCodeOf(err) != CodeUnsupportedSchema {
				t.Fatal("exact message schema was accepted", err)
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

// Characterizes how this build treats a session written by a future schema, as
// an older build would see a newer record during a mixed-version upgrade. Every
// registry-wide read skips it, and nothing rewrites the unreadable record.
func TestFutureSchemaSessionSkipsRegistryReadsWithoutWriting(t *testing.T) {
	store := fixtureStore(t)
	from, key, healthy, _ := registerPair(t, store, "codex", "claude")
	future, _ := register(t, store, "cursor")
	path, _ := store.sessionPath(future.ID)
	var record map[string]any
	if err := readJSON(path, &record); err != nil {
		t.Fatal(err)
	}
	record["schema"] = leaseSessionSchema + 1
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
	if err := store.Update(future.ID, StateBusy, false); ErrorCodeOf(err) != CodeUnsupportedSchema {
		t.Fatal("lifecycle update accepted a future session record", err)
	}
	if _, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: future.ID, MessageID: newID(), Body: "body"}); ErrorCodeOf(err) != CodeUnsupportedSchema {
		t.Fatal("send accepted a future recipient record", err)
	}
	if _, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: healthy.ID, MessageID: newID(), Body: "body"}); err != nil {
		t.Fatal("exact-ID send to a readable session failed", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("a failed read or update rewrote the future session record")
	}
}

// preLeaseSession is the session record as builds before listener leases
// (73e3050) decode and rewrite it. They reject schemas above 1 before using a
// record, and silently drop fields they do not know when they write it back.
type preLeaseSession struct {
	Schema          int                       `json:"schema"`
	NativeID        string                    `json:"native_id,omitempty"`
	AdapterState    json.RawMessage           `json:"adapter_state,omitempty"`
	ID              string                    `json:"id"`
	Kind            string                    `json:"kind"`
	Name            string                    `json:"name,omitempty"`
	NameSource      string                    `json:"name_source,omitempty"`
	NativeConfigDir string                    `json:"native_config_dir,omitempty"`
	CWD             string                    `json:"cwd"`
	Generation      string                    `json:"generation"`
	State           SessionState              `json:"state"`
	UpdatedAt       string                    `json:"updated_at"`
	Retired         bool                      `json:"retired"`
	RetiredReason   string                    `json:"retired_reason,omitempty"`
	Owner           *model.ProcessRef         `json:"owner,omitempty"`
	Liveness        string                    `json:"liveness,omitempty"`
	HookBuild       string                    `json:"hook_build,omitempty"`
	Endpoint        *model.Endpoint           `json:"endpoint,omitempty"`
	KeyHash         string                    `json:"key_hash,omitempty"`
	Receive         model.ReceiveCapabilities `json:"receive"`
}

// preLeaseRewrite reads a record as an older build would and, when it accepts
// it, writes it back as that build would after a state update.
func preLeaseRewrite(t *testing.T, store Store, id string) error {
	t.Helper()
	path, _ := store.sessionPath(id)
	var v preLeaseSession
	if err := readJSON(path, &v); err != nil {
		return err
	}
	if v.Schema < 0 || v.Schema > 1 {
		return fail(CodeUnsupportedSchema, "unsupported session schema %d", v.Schema)
	}
	v.Schema, v.State, v.UpdatedAt = 1, StateBusy, model.Timestamp()
	return writeJSON(path, v)
}

func TestLeasedRecordsAreHiddenFromOlderWriters(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	lease := armedLease(t, store, to.ID, key)
	before := sessionBytes(t, store, to.ID)
	if err := preLeaseRewrite(t, store, to.ID); ErrorCodeOf(err) != CodeUnsupportedSchema || !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
		t.Fatal("an older build could rewrite a leased record and drop its lease", err)
	}
	lease.Close(nil)
	if err := preLeaseRewrite(t, store, to.ID); err != nil {
		t.Fatal("a record without a lease was not returned to older builds", err)
	}
	current, err := store.session(to.ID)
	if err != nil || current.Listener != nil || current.State != StateBusy || current.Receive.IdleWake {
		t.Fatalf("an older build's rewrite was not readable or revived a listener: %+v %v", current, err)
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
	if err := preLeaseRewrite(t, store, to.ID); ErrorCodeOf(err) != CodeUnsupportedSchema {
		t.Fatal("an older build could rewrite an interrupted lease", err)
	}
	if grant, err := claimCursorRecovery(store, to.ID); err != nil || grant.Capability == "" {
		t.Fatal("an interrupted lease blocked recovery", err)
	}
	if err := preLeaseRewrite(t, store, to.ID); err != nil {
		t.Fatal("recovery left the record unreadable by older builds", err)
	}
}

func TestListenerLeaseUnderOlderSchemaIsRejected(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	path, _ := store.sessionPath(to.ID)
	var record map[string]any
	if err := readJSON(path, &record); err != nil {
		t.Fatal(err)
	}
	record["listener"] = model.ListenerLease{ID: newID(), Generation: to.Generation, Endpoint: model.Endpoint{Path: "/tmp/zalki-x/inbox.sock"}}
	if err := writeJSON(path, record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.session(to.ID); ErrorCodeOf(err) != CodeUnsupportedSchema {
		t.Fatal("accepted a lease stored under a schema older builds would rewrite", err)
	}
}

// A wake endpoint stored in Cursor state by a pre-lease build must not survive
// a rewrite: older builds would read it back as a live listener.
func TestPreLeaseWakeIsNotCarriedIntoRewrittenRecords(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	path, _ := store.sessionPath(to.ID)
	var record map[string]any
	if err := readJSON(path, &record); err != nil {
		t.Fatal(err)
	}
	record["adapter_state"] = map[string]any{"schema": 1, "recovery_attempted": true, "wake": model.Endpoint{Path: "/tmp/zalki-old/inbox.sock"}}
	if err := writeJSON(path, record); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(to.ID, StateBusy, false); err != nil {
		t.Fatal(err)
	}
	var written struct {
		AdapterState map[string]any `json:"adapter_state"`
	}
	if err := json.Unmarshal(sessionBytes(t, store, to.ID), &written); err != nil || written.AdapterState["wake"] != nil {
		t.Fatal("a pre-lease wake endpoint was written back for older builds", err)
	}
	if current, err := store.session(to.ID); err != nil || !cursorState(current).RecoveryAttempted || current.Receive.IdleWake {
		t.Fatal("normalizing legacy state lost recovery state or advertised a listener", err)
	}
}
