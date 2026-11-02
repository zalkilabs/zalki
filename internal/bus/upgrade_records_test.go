package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var futureExtension = json.RawMessage(`{"large":123456789012345678901234567890,"nested":[true,"future"]}`)

func addRecordExtension(t *testing.T, path string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := readJSON(path, &fields); err != nil {
		t.Fatal(err)
	}
	fields["future_extension"] = futureExtension
	if err := writeJSON(path, fields); err != nil {
		t.Fatal(err)
	}
}

func requireRecordExtension(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := readJSON(path, &fields); err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(fields["future_extension"], &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(futureExtension, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || !bytes.Contains(fields["future_extension"], []byte("123456789012345678901234567890")) {
		t.Fatal("unknown extension changed", string(fields["future_extension"]))
	}
	return fields
}

func TestRecordMergeClearsEveryOmittedKnownField(t *testing.T) {
	for _, value := range []any{Session{}, Message{}, Watch{}, Receipt{}} {
		typ := reflect.TypeOf(value)
		for i := range typ.NumField() {
			field := typ.Field(i)
			name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			if !field.IsExported() || name == "-" || !strings.Contains(options, "omitempty") {
				continue
			}
			t.Run(typ.Name()+"/"+name, func(t *testing.T) {
				previous, _ := json.Marshal(map[string]json.RawMessage{name: json.RawMessage(`{"stale":true}`), strings.ToUpper(name): json.RawMessage(`"alias"`), "future_extension": futureExtension})
				raw, err := mergeRecordJSON(previous, value)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				if _, exists := fields[name]; exists {
					t.Fatal("cleared optional field resurrected", name)
				}
				if _, exists := fields[strings.ToUpper(name)]; exists {
					t.Fatal("case variant resurrected cleared field", name)
				}
				if !bytes.Contains(fields["future_extension"], []byte("123456789012345678901234567890")) {
					t.Fatal("unknown JSON lost precision")
				}
			})
		}
	}
}

func TestSessionExtensionsSurviveLifecycleTaskApprovalLeaseAndRegistration(t *testing.T) {
	store := fixtureStore(t)
	target, key := register(t, store, "cursor")
	path := store.sessionPathMust(t, target.ID)
	addRecordExtension(t, path)
	service := &Service{Store: store}
	steps := []struct {
		name string
		run  func() error
	}{
		{"state", func() error { return store.Update(target.ID, StateBusy, false) }},
		{"task set", func() error { _, err := service.SetTask(target.ID, key, "testing upgrades"); return err }},
		{"task clear", func() error { _, err := service.SetTask(target.ID, key, ""); return err }},
		{"approval increment", func() error { return store.locked(func() error { return store.rememberApprovalHold(target.ID, 1) }) }},
		{"approval clear", func() error { return store.locked(func() error { return store.clearApprovalHeld(target.ID) }) }},
		{"name set", func() error { return store.hookName(target.ID, "fixture") }},
		{"name clear", func() error { return store.hookName(target.ID, "") }},
		{"lifecycle", func() error {
			return cursorHost(store, target.ID, "Stop").ApplyLifecycle(func(SessionView) (LifecycleChange, error) { return LifecycleChange{Transition: TransitionReady}, nil })
		}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			if err := step.run(); err != nil {
				t.Fatal(err)
			}
			requireRecordExtension(t, path)
		})
	}
	fields := requireRecordExtension(t, path)
	for _, cleared := range []string{"task", "approval_held", "name", "name_source"} {
		if _, exists := fields[cleared]; exists {
			t.Fatal("cleared field survived", cleared)
		}
	}
	lease := armedLease(t, store, target.ID, key)
	requireRecordExtension(t, path)
	lease.Close(nil)
	if _, exists := requireRecordExtension(t, path)["listener"]; exists {
		t.Fatal("released listener resurrected")
	}
	if _, _, err := store.register(target.Kind, NativeID(target), target.CWD, nil, true); err != nil {
		t.Fatal(err)
	}
	requireRecordExtension(t, path)
	if _, _, err := store.Register(target.Kind, NativeID(target), target.CWD, nil); err != nil {
		t.Fatal(err)
	}
	requireRecordExtension(t, path)
	if err := store.Update(target.ID, StateRetired, true); err != nil {
		t.Fatal(err)
	}
	requireRecordExtension(t, path)
}

func TestMessageExtensionsSurviveReceiptsAcknowledgementAndHookEmission(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	from, key, to, toKey := registerPair(t, store, "cursor", "codex")
	service.Deliver = func(_ context.Context, _ Message, _ Session) Receipt { return NewReceipt(StatusSubmitted) }
	msg, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, Body: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	addRecordExtension(t, path)
	var record map[string]json.RawMessage
	if err := readJSON(path, &record); err != nil {
		t.Fatal(err)
	}
	receipt := map[string]json.RawMessage{}
	if err := json.Unmarshal(record["receipt"], &receipt); err != nil {
		t.Fatal(err)
	}
	receipt["future_receipt"] = futureExtension
	receipt["error"] = json.RawMessage(`"clear me"`)
	record["receipt"], _ = json.Marshal(receipt)
	if err := writeJSON(path, record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(msg.ID, NewReceipt(StatusSubmitted)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ack(to.ID, toKey, msg.ID); err != nil {
		t.Fatal(err)
	}
	fields := requireRecordExtension(t, path)
	receipt = nil
	if err := json.Unmarshal(fields["receipt"], &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["future_receipt"] == nil {
		t.Fatal("nested receipt extension lost")
	}
	if _, exists := receipt["error"]; exists {
		t.Fatal("receipt error resurrected")
	}
	pending := Message{ID: newID(), To: from.ID, From: to.ID, TargetGeneration: from.Generation, Body: "pending", Status: StatusPendingActivity}
	if err := store.saveMessage(pending); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(store.Dir, "messages", pending.ID+".json")
	addRecordExtension(t, pendingPath)
	if _, err := store.EmitPending(from.ID); err != nil {
		t.Fatal(err)
	}
	requireRecordExtension(t, pendingPath)
}

func TestWatchAndFloodExtensionsSurviveRefreshAndAdmission(t *testing.T) {
	store := fixtureStore(t)
	from, _, target, _ := registerPair(t, store, "codex", "cursor")
	watch := Watch{Watcher: from.ID, Target: target.ID, CreatedAt: timestamp(), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	if err := store.saveWatch(watch); err != nil {
		t.Fatal(err)
	}
	path, _ := store.watchPath(from.ID, target.ID)
	addRecordExtension(t, path)
	watch.CreatedAt = ""
	if err := store.saveWatch(watch); err != nil {
		t.Fatal(err)
	}
	if _, exists := requireRecordExtension(t, path)["created_at"]; exists {
		t.Fatal("cleared watch timestamp resurrected")
	}
	now := time.Now()
	first := Message{ID: newID(), From: from.ID, To: target.ID}
	if err := spendFlood(store, first, now); err != nil {
		t.Fatal(err)
	}
	path, _ = store.floodPath(target.ID)
	addRecordExtension(t, path)
	before, _ := os.ReadFile(path)
	if err := spendFlood(store, first, now); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("retry rewrote quota")
	}
	if err := spendFlood(store, Message{ID: newID(), From: from.ID, To: target.ID}, now); err != nil {
		t.Fatal(err)
	}
	requireRecordExtension(t, path)
}

func TestMixedSessionScanSkipsRecordsAndPrunePreservesThem(t *testing.T) {
	store := fixtureStore(t)
	healthy, _ := register(t, store, "codex")
	future, _ := register(t, store, "cursor")
	malformed, _ := register(t, store, "cursor")
	mismatch, _ := register(t, store, "cursor")
	paths := []string{store.sessionPathMust(t, future.ID), store.sessionPathMust(t, malformed.ID), store.sessionPathMust(t, mismatch.ID)}
	rewriteRecord(t, store, future.ID, func(fields map[string]any) { fields["schema"] = 99; fields["future_field"] = true })
	if err := os.WriteFile(paths[1], []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	rewriteRecord(t, store, mismatch.ID, func(fields map[string]any) { fields["id"] = healthy.ID })
	missingID := "removed-client:" + newID()
	missingPath, _ := store.sessionRecordPath(missingID)
	if err := writeJSON(missingPath, Session{Schema: 1, ID: missingID, Kind: "removed-client", NativeID: strings.TrimPrefix(missingID, "removed-client:")}); err != nil {
		t.Fatal(err)
	}
	paths = append(paths, missingPath)
	before := map[string][]byte{}
	for _, path := range paths {
		before[path], _ = os.ReadFile(path)
	}
	rows, skipped, err := store.ListWithWarnings()
	if err != nil || len(rows) != 1 || rows[0].ID != healthy.ID || skipped != 4 {
		t.Fatal("partial scan", rows, skipped, err)
	}
	for _, dry := range []bool{true, false} {
		report, err := store.PruneRetired(dry, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, pair := range []struct{ id, reason string }{{future.ID, "session_unsupported_schema"}, {malformed.ID, "session_unreadable"}, {mismatch.ID, "session_identity_mismatch"}, {missingID, "session_adapter_missing"}} {
			if !pruneReported(report, pair.id, pair.reason) {
				t.Fatal("missing skipped record report", pair, report)
			}
		}
	}
	for _, id := range []string{future.ID, missingID} {
		report, err := store.Prune(false, []string{id})
		if err != nil || len(report.DeletedSessions) != 0 {
			t.Fatal("selected unsupported record was not skipped", report, err)
		}
	}
	for path, expected := range before {
		if got, _ := os.ReadFile(path); !bytes.Equal(got, expected) {
			t.Fatal("skipped record changed", path)
		}
	}
	if _, err := store.session(future.ID); ErrorCodeOf(err) != CodeUnsupportedSchema {
		t.Fatal("exact lookup lost schema error", err)
	}
}

func copyStateFixture(t *testing.T, name string) Store {
	t.Helper()
	store := fixtureStore(t)
	root := filepath.Join("testdata", "state", name)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		target := filepath.Join(store.Dir, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestReleasedStateFixturesRoundTripWithoutFieldLoss(t *testing.T) {
	for _, fixture := range []string{"v0.1.0-rc.5", "schema-2-lease"} {
		t.Run(fixture, func(t *testing.T) {
			store := copyStateFixture(t, fixture)
			sessions, issues, err := store.scanSessions()
			if err != nil || len(issues) != 0 || len(sessions) == 0 {
				t.Fatal("cannot read compatibility fixture", issues, err)
			}
			for _, session := range sessions {
				path := store.sessionPathMust(t, session.ID)
				var before map[string]json.RawMessage
				if err := readJSON(path, &before); err != nil {
					t.Fatal(err)
				}
				if err := store.saveSession(session); err != nil {
					t.Fatal(err)
				}
				var after map[string]json.RawMessage
				if err := readJSON(path, &after); err != nil {
					t.Fatal(err)
				}
				for key, raw := range before {
					if key == "receive" || key == "adapter_state" {
						continue
					} // Current adapter derives these known values.
					var a, b any
					if err := json.Unmarshal(raw, &a); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(after[key], &b); err != nil {
						t.Fatal("fixture field lost", key, err)
					}
					if !reflect.DeepEqual(a, b) {
						t.Fatal("fixture field changed", key)
					}
				}
			}
			if err := store.eachMessage(func(_ string, msg Message, readErr error) error {
				if readErr != nil {
					return readErr
				}
				path := filepath.Join(store.Dir, "messages", msg.ID+".json")
				var before, after any
				if err := readJSON(path, &before); err != nil {
					return err
				}
				if err := store.saveMessage(msg); err != nil {
					return err
				}
				if err := readJSON(path, &after); err != nil {
					return err
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("released message lost fields")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFutureRecordsRefuseFreshRewriteValuesAndKeepBytes(t *testing.T) {
	store := fixtureStore(t)
	from, _, target, _ := registerPair(t, store, "codex", "cursor")
	message := Message{ID: newID(), From: from.ID, To: target.ID, Status: StatusSubmitted}
	if err := store.saveMessage(message); err != nil {
		t.Fatal(err)
	}
	watch := Watch{Watcher: from.ID, Target: target.ID, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	if err := store.saveWatch(watch); err != nil {
		t.Fatal(err)
	}
	quotaMessage := Message{ID: newID(), From: from.ID, To: target.ID}
	if err := spendFlood(store, quotaMessage, time.Now()); err != nil {
		t.Fatal(err)
	}
	sessionPath := store.sessionPathMust(t, target.ID)
	watchPath, _ := store.watchPath(from.ID, target.ID)
	floodPath, _ := store.floodPath(target.ID)
	quota, err := loadFloodState(floodPath, target.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path string
		rewrite    func() error
	}{
		{"session", sessionPath, func() error { return store.saveSession(target) }},
		{"message", filepath.Join(store.Dir, "messages", message.ID+".json"), func() error { return store.saveMessage(message) }},
		{"watch", watchPath, func() error { return store.saveWatch(watch) }},
		{"flood", floodPath, func() error { return store.saveFloodState(floodPath, quota) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := readJSON(test.path, &fields); err != nil {
				t.Fatal(err)
			}
			fields["schema"], fields["future_extension"] = json.RawMessage(`99`), futureExtension
			if err := writeJSON(test.path, fields); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(test.path)
			if err := test.rewrite(); ErrorCodeOf(err) != CodeUnsupportedSchema {
				t.Fatal("fresh value overwrote future record", err)
			}
			if after, _ := os.ReadFile(test.path); !bytes.Equal(before, after) {
				t.Fatal("future bytes changed")
			}
		})
	}
}

func TestHeldExtensionsSurviveApprovalReleaseAndRetirement(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "retire"}[retire], func(t *testing.T) {
			service := newTestService(t)
			store := service.Store
			from, key, target, _ := registerPair(t, store, "cursor", "codex")
			if err := store.Update(target.ID, StateApproval, false); err != nil {
				t.Fatal(err)
			}
			msg, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: target.ID, Body: "held"})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.Dir, "messages", msg.ID+".json")
			addRecordExtension(t, path)
			addRecordExtension(t, store.sessionPathMust(t, target.ID))
			if retire {
				if _, err := store.Prune(false, []string{target.ID}); err != nil {
					t.Fatal(err)
				}
			} else {
				service.Deliver = func(context.Context, Message, Session) Receipt { return NewReceipt(StatusSubmitted) }
				if err := store.Update(target.ID, StateReady, false); err != nil {
					t.Fatal(err)
				}
				if err := service.releaseApprovalQueue(t.Context(), target.ID); err != nil {
					t.Fatal(err)
				}
			}
			fields := requireRecordExtension(t, path)
			if _, exists := fields["held"]; exists {
				t.Fatal("held flag resurrected")
			}
			requireRecordExtension(t, store.sessionPathMust(t, target.ID))
		})
	}
}

func TestFloodMergeKeepsNestedFieldsByArrivalIdentity(t *testing.T) {
	now := time.Now()
	state := floodState{Schema: 1, Target: "target", Inbound: floodBucket{Tokens: 2, At: now}, Senders: map[string]floodBucket{"kept": {Tokens: 1, At: now}, "removed": {Tokens: 1, At: now}}, Recent: []floodArrival{{ID: "first", Sender: "kept", At: now}, {ID: "second", Sender: "kept", At: now}}}
	previous, _ := json.Marshal(state)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(previous, &fields); err != nil {
		t.Fatal(err)
	}
	var arrivals []map[string]json.RawMessage
	if err := json.Unmarshal(fields["recent"], &arrivals); err != nil {
		t.Fatal(err)
	}
	arrivals[0]["future_arrival"] = json.RawMessage(`"first only"`)
	arrivals[1]["future_arrival"] = json.RawMessage(`"second only"`)
	fields["recent"], _ = json.Marshal(arrivals)
	var senders map[string]map[string]json.RawMessage
	if err := json.Unmarshal(fields["senders"], &senders); err != nil {
		t.Fatal(err)
	}
	senders["kept"]["future_bucket"] = futureExtension
	fields["senders"], _ = json.Marshal(senders)
	previous, _ = json.Marshal(fields)
	state.Recent = append(state.Recent[1:], floodArrival{ID: "third", Sender: "kept", At: now})
	delete(state.Senders, "removed")
	merged, err := mergeRecordJSON(previous, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(merged, &fields); err != nil {
		t.Fatal(err)
	}
	arrivals = nil
	if err := json.Unmarshal(fields["recent"], &arrivals); err != nil {
		t.Fatal(err)
	}
	if string(arrivals[0]["future_arrival"]) != `"second only"` || arrivals[1]["future_arrival"] != nil {
		t.Fatal("arrival extensions shifted to another reservation")
	}
	senders = nil
	if err := json.Unmarshal(fields["senders"], &senders); err != nil {
		t.Fatal(err)
	}
	if senders["kept"]["future_bucket"] == nil || senders["removed"] != nil {
		t.Fatal("bucket extensions lost or removed sender resurrected")
	}
}
