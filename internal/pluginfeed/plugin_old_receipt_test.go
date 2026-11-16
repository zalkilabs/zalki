package pluginfeed

import (
	"errors"
	"strings"
	"testing"
)

// A receipt from before zalki-plugin/1 has no manifest id: the record is an
// old record, refused without removing it, never loaded half-valid.
func TestReceiptWithoutIDIsAnOldRecord(t *testing.T) {
	for raw, old := range map[string]bool{
		`{"install":{"source":"/src/a"}}`: true,
		`{"scope":"all","install":{"source":"/src/a","id":"local/a","installed_by":"unproven","access":{}}}`: false,
		`{"scope":"all","install":{"source":"/src/a","id":"local/a"}}`:                                       true,
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

// Recovery keeps old data and offers a separate name. The source stays bounded
// and shell-quoted, and the error retains its old-record identity.
func TestPluginOldRecordRecoveryCommands(t *testing.T) {
	for raw, want := range map[string]string{
		`{"install":{"source":"/src/my plugin"}}`:  `Keep the old record and its data. To install a separate instance, choose an unused name and run: zalki plugins install '/src/my plugin' --as NEW_NAME`,
		`{"install":{"source":"/src/arbiter"}}`:    `Keep the old record and its data. To install a separate instance, choose an unused name and run: zalki plugins install /src/arbiter --as NEW_NAME`,
		`{"capabilities":{"inbox_read":true}}`:     `Keep the old record and its data. To install a separate instance, choose an unused name and run: zalki plugins install FOLDER --as NEW_NAME`,
		`{"install":{"source":"/src/a\u001b[2J"}}`: `Keep the old record and its data. To install a separate instance, choose an unused name and run: zalki plugins install FOLDER --as NEW_NAME`,
		`{"install":{"source":"src/arbiter"}}`:     `Keep the old record and its data. To install a separate instance, choose an unused name and run: zalki plugins install FOLDER --as NEW_NAME`,
		`{"install":{"source":"-rf"}}`:             `Keep the old record and its data. To install a separate instance, choose an unused name and run: zalki plugins install FOLDER --as NEW_NAME`,
	} {
		err := oldPluginRecord("arbiter-build", []byte(raw))
		if !errors.Is(err, ErrOldPluginRecord) || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("OLD_RECORD_RECOVERY_IS_NOT_PRESERVING: %s: %v, want suffix %q", raw, err, want)
		}
	}
}
