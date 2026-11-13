package pluginfeed

import "testing"

// A receipt from before zalki-plugin/1 has no manifest id: the record is an
// old record, refused with the reinstall fix, never loaded half-valid.
func TestReceiptWithoutIDIsAnOldRecord(t *testing.T) {
	for raw, old := range map[string]bool{
		`{"install":{"source":"/src/a"}}`:                              true,
		`{"scope":"all","install":{"source":"/src/a","id":"local/a"}}`: false,
		`{"scope":"none"}`: false,
		`{"install":{"source":"/src/a","id":"local/a"}}`:      true,
		`{"capabilities":{"receive":["x"]}}`:                  true,
		`{"capabilities":{"inbox_read":true}}`:                true,
		`{"install":{"source":"/src/a","id":""},"enabled":1}`: true,
	} {
		source, got := oldPluginSource([]byte(raw))
		if got != old || (got && source == "" && raw[2:9] == "install") {
			t.Errorf("%s: old = %v, source %q", raw, got, source)
		}
	}
}
