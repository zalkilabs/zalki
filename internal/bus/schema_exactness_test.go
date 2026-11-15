package bus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func priorSchemaFixture(t *testing.T, schema json.RawMessage) (Store, string, model.Message) {
	t.Helper()
	store := fixtureStore(t)
	message := model.Message{Schema: recordSchema, ID: newID(), From: "codex:" + newID(), To: "cursor:" + newID(), TargetGeneration: newID(), CreatedAt: model.Timestamp(), Status: model.StatusSubmitted, Body: "before"}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if schema == nil {
		delete(fields, "schema")
	} else {
		fields["schema"] = schema
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "messages", message.ID+".json")
	if err := privateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Unix(1000, 0), time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	return store, path, message
}

func TestNonCurrentPriorImagesAreNeverOverwritten(t *testing.T) {
	for _, test := range []struct {
		name   string
		schema json.RawMessage
	}{
		{"missing", nil}, {"null", json.RawMessage(`null`)}, {"zero", json.RawMessage(`0`)}, {"negative", json.RawMessage(`-1`)}, {"newer", json.RawMessage(`2`)},
	} {
		t.Run(test.name, func(t *testing.T) { checkPriorSchemaRefusal(t, test.schema) })
	}
}

func checkPriorSchemaRefusal(t *testing.T, schema json.RawMessage) {
	t.Helper()
	store, path, next := priorSchemaFixture(t, schema)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	next.Body = "after"
	writeErr := store.locked(func() error { return store.writeRecord(path, next) })
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || !afterInfo.ModTime().Equal(info.ModTime()) {
		t.Fatal("noncurrent prior image was overwritten")
	}
	if model.ErrorCodeOf(writeErr) != model.CodeUnsupportedSchema {
		t.Fatal("noncurrent prior image was not refused", writeErr)
	}
}

func TestCurrentSchemaPriorImageCanBeRewritten(t *testing.T) {
	store, path, next := priorSchemaFixture(t, json.RawMessage(`1`))
	next.Body = "after"
	if err := store.locked(func() error { return store.writeRecord(path, next) }); err != nil {
		t.Fatal(err)
	}
	var saved model.Message
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Schema != recordSchema || saved.Body != "after" {
		t.Fatal("current prior image was not rewritten", err)
	}
}
