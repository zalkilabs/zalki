package pluginfeed

import (
	"strings"
	"testing"
)

// A receipt from before zalki-plugin/1 has no manifest id: the record is an
// old record, refused with the reinstall fix, never loaded half-valid.
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

// U43: the fix line for an old record is the exact remove and install
// commands: the receipt's source, shell-quoted, and --as NAME, so the
// instance keeps its name even when the manifest's default name differs.
func TestPluginOldRecordRecoveryCommands(t *testing.T) {
	for raw, want := range map[string]string{
		`{"install":{"source":"/src/my plugin"}}`:  `Run: zalki plugins remove arbiter-build && zalki plugins install '/src/my plugin' --as arbiter-build`,
		`{"install":{"source":"/src/arbiter"}}`:    `Run: zalki plugins remove arbiter-build && zalki plugins install /src/arbiter --as arbiter-build`,
		`{"capabilities":{"inbox_read":true}}`:     `Run: zalki plugins remove arbiter-build && zalki plugins install FOLDER --as arbiter-build`,
		`{"install":{"source":"/src/a\u001b[2J"}}`: `Run: zalki plugins remove arbiter-build && zalki plugins install FOLDER --as arbiter-build`,
		`{"install":{"source":"src/arbiter"}}`:     `Run: zalki plugins remove arbiter-build && zalki plugins install FOLDER --as arbiter-build`,
		`{"install":{"source":"-rf"}}`:             `Run: zalki plugins remove arbiter-build && zalki plugins install FOLDER --as arbiter-build`,
	} {
		err := oldPluginRecord("arbiter-build", []byte(raw))
		if err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%s: %v, want suffix %q", raw, err, want)
		}
	}
}
