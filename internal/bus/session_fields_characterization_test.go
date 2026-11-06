// These in-package checks pin adapter ownership independently of reflected fields.
// They run before and after Session becomes a model alias.
package bus

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSessionCoreFieldsAndSanitization(t *testing.T) {
	want := []string{"schema", "native_id", "adapter_state", "id", "kind", "name", "name_source", "task", "native_config_dir", "cwd", "generation", "state", "updated_at", "retired", "retired_reason", "approval_held", "owner", "liveness", "hook_build", "endpoint", "listener", "key_hash", "receive"}
	if !reflect.DeepEqual(sessionFields, want) {
		t.Fatalf("reflected core fields=%v, want all 23 fields in order: %v", sessionFields, want)
	}
	fields := map[string]json.RawMessage{"client_extension": json.RawMessage(`{"n":1}`)}
	for _, key := range want {
		fields[key] = json.RawMessage(`"core secret"`)
		fields[strings.ToUpper(key)] = json.RawMessage(`"upper core secret"`)
	}
	record, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	extensions, err := stateExtensions(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(extensions) != 1 || string(extensions["client_extension"]) != `{"n":1}` {
		t.Fatalf("adapter extensions expose core fields or lose client data: %v", extensions)
	}
}
