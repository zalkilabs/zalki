package pluginfeed

import (
	"reflect"
	"testing"
)

// Every access field needs an explicit message-privacy decision. A future field
// cannot silently bypass the predicate shared by the host and owner grants.
func TestPluginAccessMessagesClassifiesEveryField(t *testing.T) {
	policy := map[string]bool{
		"Status":  false,
		"Receive": true,
		"Send":    true,
		"Read":    true,
	}
	accessType := reflect.TypeOf(PluginAccess{})
	for i := 0; i < accessType.NumField(); i++ {
		field := accessType.Field(i)
		want, listed := policy[field.Name]
		if !listed {
			t.Fatalf("UNLISTED_PLUGIN_ACCESS_FIELD: %s", field.Name)
		}
		access := accessWithOnlyMessagePolicyField(t, field)
		if got := access.Messages(); got != want {
			t.Fatalf("MESSAGE_ACCESS_FIELD_MISCLASSIFIED: %s got %v, want %v", field.Name, got, want)
		}
	}
	for name := range policy {
		if _, exists := accessType.FieldByName(name); !exists {
			t.Fatalf("STALE_PLUGIN_ACCESS_FIELD_POLICY: %s", name)
		}
	}
	if (PluginAccess{}).Messages() {
		t.Fatal("EMPTY_ACCESS_HAS_MESSAGES")
	}
}

func accessWithOnlyMessagePolicyField(t *testing.T, field reflect.StructField) PluginAccess {
	t.Helper()
	var access PluginAccess
	value := reflect.ValueOf(&access).Elem().FieldByIndex(field.Index)
	if !value.CanSet() {
		t.Fatalf("PLUGIN_ACCESS_FIELD_FIXTURE_NOT_SETTABLE: %s", field.Name)
	}
	switch value.Kind() {
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Slice:
		if value.Type().Elem().Kind() != reflect.String {
			t.Fatalf("PLUGIN_ACCESS_FIELD_FIXTURE_TYPE: %s", field.Name)
		}
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
		value.Index(0).SetString("codex:00000000-0000-4000-8000-000000000001")
	default:
		t.Fatalf("PLUGIN_ACCESS_FIELD_FIXTURE_TYPE: %s", field.Name)
	}
	return access
}
