package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestRuntimeRecordHeaderIsPrivateAndExact(t *testing.T) {
	s := fixtureStore(t)
	lease, err := s.TrackRuntime("mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	public, err := json.Marshal(lease.Info())
	if err != nil || bytes.Contains(public, []byte(`"schema"`)) {
		t.Fatal("RUNTIME_PUBLIC_PROJECTION_GREW_SCHEMA", err)
	}
	path := filepath.Join(s.dir, "runtimes", lease.Info().ID+".json")
	raw := readCommitFile(t, path)
	var header struct{ Schema int }
	if err := json.Unmarshal(raw, &header); err != nil || header.Schema != recordSchema {
		t.Fatal("RUNTIME_FILE_STAMP_MISSING", err, header.Schema)
	}
	for _, schema := range []string{"missing", "0", "null", "2", "-1"} {
		t.Run(schema, func(t *testing.T) { checkRuntimeHeaderRefusal(t, s, path, raw, schema) })
	}
}

func checkRuntimeHeaderRefusal(t *testing.T, s Store, path string, raw []byte, schema string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if schema == "missing" {
		delete(fields, "schema")
	} else {
		fields["schema"] = json.RawMessage(schema)
	}
	before, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	// Direct read refuses; aggregate scan counts and skips unchanged.
	var refused RuntimeInfo
	if err := readRuntimeRecord(path, &refused); model.ErrorCodeOf(err) != model.CodeUnsupportedSchema {
		t.Fatal("RUNTIME_OLD_HEADER_WAS_READ", err)
	}
	if rows, skipped, err := s.rawRuntimes(); err != nil || skipped != 1 || len(rows) != 0 {
		t.Fatal("RUNTIME_OLD_HEADER_SCAN_DID_NOT_SKIP", rows, skipped, err)
	}
	if after := readCommitFile(t, path); !bytes.Equal(before, after) {
		t.Fatal("RUNTIME_HEADER_REFUSAL_CHANGED_RECORD")
	}
}
