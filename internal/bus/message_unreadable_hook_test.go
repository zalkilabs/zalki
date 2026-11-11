package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// One unreadable message file must not block hook delivery for any session.
// The bad file stays on disk (persisted poison) and is reported, never repaired.

func capture(store *Store) *bytes.Buffer {
	var output bytes.Buffer
	store.Logger = slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &output
}

// skippedRecords returns the message.unreadable_skipped records in output.
func skippedRecords(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "message.unreadable_skipped" {
			records = append(records, record)
		}
	}
	return records
}

func requireOneSkipRecord(t *testing.T, output *bytes.Buffer, actor, id string, count float64) {
	t.Helper()
	records := skippedRecords(t, output)
	if len(records) != 1 {
		t.Fatalf("want one skip diagnostic, got %d: %s", len(records), output)
	}
	record := records[0]
	if record["message_id"] != id || record["skipped"] != count || record["actor"] != actor || record["level"] != "WARN" {
		t.Fatalf("skip diagnostic = %v", record)
	}
	for _, key := range []string{"body", "path", "error", "err"} {
		if _, ok := record[key]; ok {
			t.Fatalf("skip diagnostic has forbidden key %q: %v", key, record)
		}
	}
}

func TestEmitPendingSkipsAnUnreadableFileAndStillDelivers(t *testing.T) {
	for _, p := range unreadableMessages() {
		t.Run(p.name, func(t *testing.T) {
			f := newEmitFixture(t)
			output := capture(&f.store)
			base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			first := f.queue(t, "first", base)
			second := f.queue(t, "second", base.Add(time.Second))
			bad := newUnreadableMessage(t, f.store, p)
			// The poison persists across calls; the second call emits nothing new.
			text := emitSkipping(t, f, output, bad)
			if got := emitted(t, text, []model.Message{first, second}); strings.Join(got, ",") != first.ID+","+second.ID {
				t.Fatalf("emitted %v, want first then second", got)
			}
			if text := emitSkipping(t, f, output, bad); text != "" {
				t.Fatalf("second call emitted again: %q", text)
			}
			for _, msg := range []model.Message{first, second} {
				if current := f.read(t, msg.ID); current.Status != model.StatusContextEmitted {
					t.Fatalf("readable message %s status %q", msg.ID, current.Status)
				}
			}
			if _, err := os.Lstat(filepath.Join(f.store.Dir, "messages", bad+".json")); err != nil {
				t.Fatal("the bad file was removed or repaired", err)
			}
		})
	}
}

// emitSkipping runs one hook call that must succeed and report bad once.
func emitSkipping(t *testing.T, f emitFixture, output *bytes.Buffer, bad string) string {
	t.Helper()
	output.Reset()
	text, err := f.store.EmitPending(f.target.ID)
	if err != nil {
		t.Fatalf("EmitPending failed on one bad file: %v", err)
	}
	requireOneSkipRecord(t, output, f.target.ID, bad, 1)
	return text
}

// A bad file under a name that is not a UUID has no open marker, so the hook
// never reads it. The list scan counts it but does not log its name.
func TestEmitPendingDiagnosticNeverLogsANonCanonicalName(t *testing.T) {
	f := newOpenFixture(t)
	output := capture(&f.store)
	if err := os.WriteFile(filepath.Join(f.store.Dir, "messages", "secret-name.json"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EmitPending(f.target.ID); err != nil {
		t.Fatal(err)
	}
	if records := skippedRecords(t, output); len(records) != 0 {
		t.Fatalf("the hook read a file with no marker: %v", records)
	}
	if _, err := f.store.ReadMessages(MessageQuery{Actor: f.target.ID, Capability: f.targetKey}); err != nil {
		t.Fatal(err)
	}
	records := skippedRecords(t, output)
	if len(records) != 1 || records[0]["skipped"] != float64(1) {
		t.Fatalf("records = %v", records)
	}
	if _, ok := records[0]["message_id"]; ok || strings.Contains(output.String(), "secret-name") {
		t.Fatalf("a non-canonical file name was logged: %s", output)
	}
}

// Many bad files still give exactly one diagnostic per scan, with the count.
func TestEmitPendingReportsAllSkippedFilesInOneDiagnostic(t *testing.T) {
	f := newEmitFixture(t)
	output := capture(&f.store)
	var first string
	for i := 0; i < 3; i++ {
		id := newUnreadableMessage(t, f.store, truncatedMessage)
		if first == "" || id < first {
			first = id // eachMessage visits files in lexical order.
		}
	}
	if _, err := f.store.EmitPending(f.target.ID); err != nil {
		t.Fatal(err)
	}
	requireOneSkipRecord(t, output, f.target.ID, first, 3)
}

// A message that becomes unreadable is never marked emitted or acknowledged,
// and becomes deliverable again once it is readable.
func TestEmitPendingNeverMarksASkippedMessageEmitted(t *testing.T) {
	f := newEmitFixture(t)
	msg := f.queue(t, "addressed to this session", time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(f.store.Dir, "messages", msg.ID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)/2], 0600); err != nil {
		t.Fatal(err)
	}
	if text, err := f.store.EmitPending(f.target.ID); err != nil || text != "" {
		t.Fatalf("EmitPending = %q, %v", text, err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, raw[:len(raw)/2]) {
		t.Fatal("the skipped record was rewritten", err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if current := f.read(t, msg.ID); current.Status != emitHookPending || current.AcknowledgedAt != "" {
		t.Fatalf("skipped message status %q acked %q", current.Status, current.AcknowledgedAt)
	}
	text, err := f.store.EmitPending(f.target.ID)
	if err != nil || !strings.Contains(text, msg.ID) {
		t.Fatalf("repaired message not delivered: %q %v", text, err)
	}
}

// heldFixture is a hook-delivery target with one message held for its approval prompt.
func heldFixture(t *testing.T) (Service, model.Session, model.Message) {
	t.Helper()
	service := newTestService(t)
	sender, key, target, _ := registerPair(t, service.Store, "codex", "cursor")
	if err := service.Store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	held, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "waits for the prompt"})
	if err != nil || !held.Held {
		t.Fatalf("setup: %+v %v", held, err)
	}
	if err := service.Store.Update(target.ID, model.StateReady, false); err != nil {
		t.Fatal(err)
	}
	return service, target, held
}

func approvalHeld(t *testing.T, store Store, id string) int {
	t.Helper()
	current, err := store.session(id)
	if err != nil {
		t.Fatal(err)
	}
	return current.ApprovalHeld
}

// Cursor hook poll: the held message is released and emitted. The skipped file
// is not a hold, so the count drops to 0 (see heldAfterScan).
func TestEmitPendingReleasesReadableApprovalsWhenSkipping(t *testing.T) {
	service, target, held := heldFixture(t)
	output := capture(&service.Store)
	bad := newUnreadableMessage(t, service.Store, truncatedMessage)
	text, err := service.Store.EmitPending(target.ID)
	if err != nil || !strings.Contains(text, held.ID) {
		t.Fatalf("EmitPending = %q, %v", text, err)
	}
	if got := approvalHeld(t, service.Store, target.ID); got != 0 {
		t.Fatalf("held count = %d after the only hold was released", got)
	}
	// Approval scan and pending scan each report once.
	if records := skippedRecords(t, output); len(records) != 2 || records[0]["message_id"] != bad || records[1]["message_id"] != bad {
		t.Fatalf("records = %v", records)
	}
}

// Every agent's lifecycle hook (Claude, Codex, Cursor) runs releaseApprovalQueue.
// A hold whose file is unreadable keeps its credit; it is released once readable.
func TestLifecycleApprovalReleaseSkipsAnUnreadableHold(t *testing.T) {
	service, target, held := heldFixture(t)
	second := holdAnother(t, service, target)
	output := capture(&service.Store)
	original := writeUnreadableMessage(t, service.Store, second.ID, halfMessage)
	if err := service.releaseApprovalQueue(context.Background(), target.ID); err != nil {
		t.Fatalf("lifecycle release failed on one bad file: %v", err)
	}
	if current := storedMessage(t, service.Store, held.ID); current.Held || current.Status != model.StatusPendingActivity {
		t.Fatalf("held message after release = %+v", current)
	}
	if got := approvalHeld(t, service.Store, target.ID); got != 1 {
		t.Fatalf("held count = %d, want 1 for the unreadable hold", got)
	}
	requireOneSkipRecord(t, output, target.ID, second.ID, 1)
	restoreMessage(t, service.Store, second.ID, original)
	if err := service.releaseApprovalQueue(context.Background(), target.ID); err != nil {
		t.Fatal(err)
	}
	if current := storedMessage(t, service.Store, second.ID); current.Held || current.Status != model.StatusPendingActivity {
		t.Fatalf("repaired hold after release = %+v", current)
	}
	if got := approvalHeld(t, service.Store, target.ID); got != 0 {
		t.Fatalf("held count = %d after the repaired hold was released", got)
	}
}

// SessionStart of a known session (resume) settles its old approval queue.
func TestRegistrationOfAKnownSessionSkipsAnUnreadableFile(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "cursor"} {
		t.Run(kind, func(t *testing.T) {
			store := fixtureStore(t)
			session, _ := register(t, store, kind)
			output := capture(&store)
			bad := newUnreadableMessage(t, store, truncatedMessage)
			var endpoint *model.Endpoint
			if kind == "claude" {
				endpoint = &model.Endpoint{Path: "/fixture/not-a-real-socket"}
			}
			again, _, err := store.Register(kind, session.NativeID, "/workspace", endpoint)
			if err != nil {
				t.Fatalf("re-registration failed on one bad file: %v", err)
			}
			if again.Generation == session.Generation {
				t.Fatal("re-registration kept the generation")
			}
			requireOneSkipRecord(t, output, session.ID, bad, 1)
		})
	}
}

// Strict readers are unchanged: an exact read of the bad file still fails.
func TestExactReadOfAnUnreadableFileStillFails(t *testing.T) {
	store := fixtureStore(t)
	register(t, store, "codex")
	bad := newUnreadableMessage(t, store, truncatedMessage)
	if _, err := store.message(bad); err == nil {
		t.Fatal("exact read of a truncated record succeeded")
	}
}

// Prune already reports unreadable files as message_unreadable, so its own
// approval settlement adds no second diagnostic.
func TestPruneApprovalSettlementAddsNoSkipDiagnostic(t *testing.T) {
	service, target, held := heldFixture(t)
	store := service.Store
	output := capture(&store)
	newUnreadableMessage(t, store, truncatedMessage)
	if err := store.locked(func() error {
		return store.settleReadableApprovalQueue(target.ID, "destination has retired")
	}); err != nil {
		t.Fatal(err)
	}
	if current := storedMessage(t, store, held.ID); current.Held || current.Status != model.StatusNotSent {
		t.Fatalf("readable held message not settled: %+v", current)
	}
	if records := skippedRecords(t, output); len(records) != 0 {
		t.Fatalf("prune settlement logged a skip diagnostic: %v", records)
	}
}

// Cross-recipient: A sends one message to B and one to C, then C's file breaks.
// B's hook still delivers B's message and never shows C's ID; C's hook works
// and only the diagnostic shows the problem.
func TestUnreadableFileOfAnotherRecipientDoesNotBlockHooks(t *testing.T) {
	for _, p := range []unreadableFile{truncatedMessage, modeZeroMessage} {
		t.Run(p.name, func(t *testing.T) {
			store := fixtureStore(t)
			output := capture(&store)
			a, key := register(t, store, "codex")
			b, _ := register(t, store, "cursor")
			c, _ := register(t, store, "cursor")
			fb := emitFixture{store: store, target: b, sender: a, key: key}
			fc := emitFixture{store: store, target: c, sender: a, key: key}
			base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			toC := fc.queue(t, "for C", base)
			toB := fb.queue(t, "for B", base.Add(time.Second))
			writeUnreadableMessage(t, store, toC.ID, p)
			text := emitSkipping(t, fb, output, toC.ID)
			if !strings.Contains(text, toB.ID) || strings.Contains(text, toC.ID) || strings.Contains(text, "for C") {
				t.Fatalf("B's hook context = %q", text)
			}
			if current := fb.read(t, toB.ID); current.Status != model.StatusContextEmitted {
				t.Fatalf("B's message status %q", current.Status)
			}
			if text := emitSkipping(t, fc, output, toC.ID); text != "" {
				t.Fatalf("C's hook emitted from an unreadable file: %q", text)
			}
		})
	}
}
