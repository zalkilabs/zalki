package pluginfeed

import (
	"fmt"
	"testing"
)

func TestLegacyReceiptClassificationIgnoresEnabledExtraType(t *testing.T) {
	for _, suffix := range []string{"", `,"enabled":true`, `,"enabled":1`, `,"enabled":null`, `,"enabled":"true"`, `,"enabled":  true  `} {
		raw := []byte(fmt.Sprintf(`{"scope":"all","install":{"source":"/src/a","id":"","installed_by":"unproven","access":{}}%s}`, suffix))
		source, old := oldPluginSource(raw)
		if !old || source != "/src/a" {
			t.Fatal("F_LEGACY_ENABLED_EXTRA_FIELD_NEVER_HIDES_RECEIPT", suffix, source, old)
		}
	}
}
func TestOnlyLiteralEnabledTrueRequiresEnableIncarnation(t *testing.T) {
	for _, tc := range []struct {
		suffix string
		old    bool
	}{{"", false}, {`,"enabled":true`, true}, {`,"enabled":1`, false}, {`,"enabled":null`, false}, {`,"enabled":"true"`, false}, {`,"enabled":  true  `, true}} {
		raw := []byte(fmt.Sprintf(`{"scope":"all","install":{"source":"/src/current","id":"local/a","installed_by":"unproven","access":{}}%s}`, tc.suffix))
		source, old := oldPluginSource(raw)
		if old != tc.old || source != "/src/current" {
			t.Fatal("F_ONLY_LITERAL_TRUE_CLOSES_MISSING_ENABLE_EPOCH", tc.suffix, source, old)
		}
	}
}
