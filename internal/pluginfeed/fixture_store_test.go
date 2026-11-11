package pluginfeed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// These are persisted-format fixtures, not new production Store APIs.
// Canonical message and event writer byte checks remain in bus.
const (
	recordSchema      = 1
	maxStateFileBytes = bus.MaxMessageRecordBytes
	maxEventBytes     = 4096
	eventMetaPath     = "events/meta.json"
)

func readCommitFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeJSON(path string, value any) error {
	raw, err := journal.EncodeJSON(value, bus.MaxMessageRecordBytes)
	if err != nil {
		return err
	}
	return journal.WriteAtomic(path, raw)
}

func sessionTestPath(store bus.Store, id string) (string, error) {
	if err := model.ValidateSessionAddress(id); err != nil {
		return "", err
	}
	kind, _, _ := strings.Cut(id, ":")
	if _, err := store.Registry.Lookup(kind); err != nil {
		return "", err
	}
	return filepath.Join(store.Dir, "sessions", strings.ReplaceAll(id, ":", "_")+".json"), nil
}

func saveTestMessage(store bus.Store, message model.Message) error {
	if message.Schema < 0 || message.Schema > recordSchema {
		return model.NewError(model.CodeUnsupportedSchema, "unsupported message schema %d", message.Schema)
	}
	if _, err := model.UUID(message.ID); err != nil {
		return err
	}
	message.Schema = recordSchema
	path := filepath.Join(store.Dir, "messages", message.ID+".json")
	previous, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged, err := journal.Merge(previous, message)
	if err != nil {
		return err
	}
	return writeJSON(path, merged)
}

func journalSnapshot(t *testing.T, store bus.Store) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	if err := filepath.WalkDir(store.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(store.Dir, path)
		if err == nil {
			snapshot[rel] = string(raw)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

type eventTestMetadata struct {
	Epoch string `json:"epoch"`
	First uint64 `json:"first_sequence,string"`
	Last  uint64 `json:"last_sequence,string"`
	Count uint64 `json:"count"`
	Bytes uint64 `json:"bytes"`
}

func eventRecordPath(sequence uint64) string {
	return filepath.Join("events", "records", fmt.Sprintf("%020d.json", sequence))
}

func eventState(t *testing.T, store bus.Store) eventTestMetadata {
	t.Helper()
	var head eventTestMetadata
	if err := journal.ReadJSON(filepath.Join(store.Dir, eventMetaPath), bus.MaxMessageRecordBytes, &head); err != nil {
		t.Fatal(err)
	}
	return head
}

// Feed policy needs a small retained window. Actual commit retention and all
// redo/sync cut points remain tested against the private writer in bus.
func setEventTestState(t *testing.T, store bus.Store, id string, state model.SessionState) {
	t.Helper()
	if err := store.Update(id, state, false); err != nil {
		t.Fatal(err)
	}
	head := eventState(t, store)
	for head.Count > 2 {
		path := filepath.Join(store.Dir, eventRecordPath(head.First))
		raw := readCommitFile(t, path)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		head.First++
		head.Count--
		head.Bytes -= uint64(len(raw))
	}
	if err := writeJSON(filepath.Join(store.Dir, eventMetaPath), head); err != nil {
		t.Fatal(err)
	}
}

func testCoveredUpdate(t *testing.T, store bus.Store, id string) {
	t.Helper()
	setEventTestState(t, store, id, model.StateReady)
	setEventTestState(t, store, id, model.StateBusy)
}

type recoveryProbeAdapter struct {
	stubCursorAdapter
	grant *agent.RecoveryGrant
}

func (a recoveryProbeAdapter) HandleHook(host agent.HookHost, _ agent.HookInput, _ string, _ io.Writer) (bool, error) {
	grant, err := host.(agent.LifecycleHost).ClaimRecovery(stubClaimRecovery)
	*a.grant = grant
	return true, err
}

func claimCursorRecovery(store bus.Store, actor string) (agent.RecoveryGrant, error) {
	var grant agent.RecoveryGrant
	registry, err := agent.NewRegistry(stubCodexAdapter{}, stubClaudeAdapter{}, recoveryProbeAdapter{grant: &grant})
	if err != nil {
		return grant, err
	}
	store.Registry = registry
	_, native, _ := strings.Cut(actor, ":")
	input, err := json.Marshal(agent.HookInput{SessionID: native, CWD: "/workspace", Event: "postToolUse"})
	if err != nil {
		return grant, err
	}
	service := bus.Service{Store: store}
	err = service.Hook(context.Background(), "cursor", bytes.NewReader(input), io.Discard)
	return grant, err
}
