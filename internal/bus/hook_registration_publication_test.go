package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

type registrationPublication struct {
	dir, actor string
	record     map[string]any
	event      eventRecord
	err        error
	count      int
}

func (*registrationPublication) Enabled(context.Context, slog.Level) bool { return true }
func (h *registrationPublication) WithAttrs([]slog.Attr) slog.Handler     { return h }
func (h *registrationPublication) WithGroup(string) slog.Handler          { return h }
func (h *registrationPublication) Handle(_ context.Context, entry slog.Record) error {
	if entry.Message != "session.registered" {
		return nil
	}
	h.count++
	path := filepath.Join(h.dir, "sessions", strings.ReplaceAll(h.actor, ":", "_")+".json")
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &h.record)
	}
	if err == nil {
		raw, err = os.ReadFile(filepath.Join(h.dir, eventRecordPath(1)))
		if err == nil {
			err = decodeEventJSON(raw, &h.event)
		}
	}
	h.err = err
	return err
}

// Drive the real service paths, capturing the first registration after-image
// at publication and the immutable first event, then compare every final field.
// The constructor guard below prevents moving initialization after publication.
func assertPromptInitializationParity(t *testing.T, policy string) {
	t.Helper()
	t.Setenv("ZALKI_PLUGINS", policy)
	native := newID()
	one, pubOne := runRegistrationPublication(t, native, "sessionStart")
	two, pubTwo := runRegistrationPublication(t, native, "beforeSubmitPrompt")
	assertSessionPluginPublication(t, pubOne.record, policy)
	assertSessionPluginPublication(t, pubTwo.record, policy)
	firstA := normalizeRegistrationRecord(t, pubOne.record, false)
	firstB := normalizeRegistrationRecord(t, pubTwo.record, false)
	if !reflect.DeepEqual(firstA, firstB) {
		t.Fatal("C16_FIRST_PUBLISHED_FULL_RECORDS_MATCH")
	}
	if normalizeRegistrationEvent(pubOne.event.Event) != normalizeRegistrationEvent(pubTwo.event.Event) {
		t.Fatal("C16_FIRST_PUBLISHED_EVENTS_MATCH")
	}
	finalA := readPublishedRegistration(t, one, pubOne.actor)
	finalB := readPublishedRegistration(t, two, pubTwo.actor)
	if !reflect.DeepEqual(normalizeRegistrationRecord(t, finalA, true), normalizeRegistrationRecord(t, finalB, true)) {
		t.Fatal("C16_ALL_FINAL_FIELDS_SHARED")
	}
	if !reflect.DeepEqual(normalizeRegistrationRecord(t, pubOne.record, true), normalizeRegistrationRecord(t, finalA, true)) || !reflect.DeepEqual(normalizeRegistrationRecord(t, pubTwo.record, true), normalizeRegistrationRecord(t, finalB, true)) {
		t.Fatal("C16_ALL_INITIAL_FIELDS_PRECEDE_FIRST_PUBLICATION")
	}
}

func runRegistrationPublication(t *testing.T, native, event string) (Store, *registrationPublication) {
	t.Helper()
	store := fixtureStore(t)
	publication := &registrationPublication{dir: store.Dir, actor: "cursor:" + native}
	store.Logger = slog.New(publication)
	input := `{"conversation_id":"` + native + `","session_id":"` + native + `","cwd":"/workspace","session_title":"First publication","hook_event_name":"` + event + `"}`
	if err := (&Service{Store: store}).Hook(t.Context(), "cursor", strings.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if publication.err != nil || publication.count != 1 || publication.record == nil || publication.event.Event.Type != model.EventSessionRegistered {
		t.Fatal("C16_REAL_HOOK_PUBLISHED_ONE_INITIAL_RECORD_AND_EVENT", publication.err)
	}
	return store, publication
}

func readPublishedRegistration(t *testing.T, store Store, actor string) map[string]any {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal(sessionBytes(t, store, actor), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func normalizeRegistrationRecord(t *testing.T, record map[string]any, lifecycle bool) map[string]any {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"generation", "key_hash", "updated_at"} {
		delete(cloned, key)
	}
	if policy, ok := cloned["plugin_policy"].(map[string]any); ok {
		if audit, ok := policy["last_change"].(map[string]any); ok {
			delete(audit, "id")
			delete(audit, "at")
		}
	}
	if lifecycle {
		cloned["state"] = "ready"
	}
	return cloned
}

func normalizeRegistrationEvent(event model.Event) model.Event {
	event.ID, event.OccurredAt, event.TransactionID, event.Session.Generation = "", "", "", ""
	return event
}

func TestPromptRegistrationConstructorPublicationGuard(t *testing.T) {
	for file, function := range map[string]string{"session.go": "register", "hook_registration.go": "registerPromptAbsent"} {
		fn := registrationFunction(t, file, function)
		constructor, publish := registrationCall(fn, "newRegistration"), registrationCall(fn, "saveSessionFor")
		if !constructor.IsValid() || !publish.IsValid() || constructor >= publish {
			t.Fatal("C16_SHARED_CONSTRUCTOR_BEFORE_PUBLICATION", function)
		}
	}
	constructor := registrationFunction(t, "session.go", "newRegistration")
	if !registrationCall(constructor, "initializeHookRegistration").IsValid() {
		t.Fatal("C16_INITIALIZATION_BELONGS_TO_SHARED_CONSTRUCTOR")
	}
	if registrationCall(constructor, "saveSessionFor").IsValid() {
		t.Fatal("C16_CONSTRUCTOR_CANNOT_PUBLISH_PARTIAL_STATE")
	}
}

func registrationFunction(t *testing.T, path, name string) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatal("C16_MISSING_REGISTRATION_FUNCTION", name)
	return nil
}

func registrationCall(fn *ast.FuncDecl, name string) token.Pos {
	var position token.Pos
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if target, ok := call.Fun.(*ast.Ident); ok && target.Name == name && !position.IsValid() {
			position = call.Pos()
		}
		return true
	})
	return position
}
