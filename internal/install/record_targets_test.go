package install

import (
	"strings"
	"testing"
)

const (
	testHome  = "/home/user"
	testState = "/home/user/state"
)

func recordBody(extra string) string {
	return `{"schema":3,"home":"` + testHome + `","state":"` + testState + `","targets":{".a/hooks.json":"/x/hooks.json","bad":5}` + extra + `}`
}

func TestRecordedTargets(t *testing.T) {
	for _, test := range []struct {
		name, record string
		reason       string
		targets      map[string]string
	}{
		{"belongs to this installation", recordBody(""), "", map[string]string{".a/hooks.json": "/x/hooks.json"}},
		{"another schema", `{"schema":2,"home":"` + testHome + `","state":"` + testState + `","targets":{}}`, "ambiguous_ownership", nil},
		{"another home", `{"schema":3,"home":"/other","state":"` + testState + `","targets":{}}`, "ambiguous_ownership", nil},
		{"another state directory", `{"schema":3,"home":"` + testHome + `","state":"/other","targets":{}}`, "ambiguous_ownership", nil},
		{"no targets", `{"schema":3,"home":"` + testHome + `","state":"` + testState + `"}`, "ambiguous_ownership", nil},
		{"targets that is not an object", `{"schema":3,"home":"` + testHome + `","state":"` + testState + `","targets":[]}`, "ambiguous_ownership", nil},
		{"a schema that is not a number", `{"schema":"3","home":"` + testHome + `","state":"` + testState + `","targets":{}}`, "ambiguous_ownership", nil},
		{"not JSON", `{broken`, "invalid_data", nil},
		{"not an object", `[1]`, "invalid_data", nil},
		{"empty", ``, "invalid_data", nil},
		{"trailing data", recordBody("") + ` {}`, "invalid_data", nil},
		{"not UTF-8", "{\"schema\":3,\"home\":\"\xff\"}", "invalid_data", nil},
		// The six bodies encoding/json would accept but the strict reader refuses.
		{"a duplicate key", `{"schema":3,"schema":3,"home":"` + testHome + `","state":"` + testState + `","targets":{}}`, "invalid_data", nil},
		{"a duplicate key in targets", recordBody("")[:len(recordBody(""))-1] + `,"targets":{}}`, "invalid_data", nil},
		{"an unpaired high surrogate", recordBody(`,"x":"\ud800"`), "invalid_data", nil},
		{"an unpaired low surrogate", recordBody(`,"x":"\udc00"`), "invalid_data", nil},
		{"nesting of 70 levels", recordBody(`,"x":` + strings.Repeat("[", 70) + strings.Repeat("]", 70)), "invalid_data", nil},
		{"a number over 64 bits", recordBody(`,"x":18446744073709551616`), "invalid_data", nil},
		{"a number that is not an integer", `{"schema":3.5,"home":"` + testHome + `","state":"` + testState + `","targets":{}}`, "invalid_data", nil},
	} {
		targets, reason := RecordedTargets([]byte(test.record), "", testHome, testState)
		if reason != test.reason || len(targets) != len(test.targets) || (len(targets) == 1 && targets[".a/hooks.json"] != "/x/hooks.json") {
			t.Fatalf("%s: targets %v reason %q, want %v %q", test.name, targets, reason, test.targets, test.reason)
		}
	}
}

// A reason from the read comes back unchanged and the bytes are not looked at.
func TestRecordedTargetsReturnsTheReadReason(t *testing.T) {
	for _, reason := range []string{"missing_file", "unsafe_file", "invalid_data"} {
		if targets, got := RecordedTargets([]byte(recordBody("")), reason, testHome, testState); got != reason || targets != nil {
			t.Fatalf("read reason %q gave %v %q", reason, targets, got)
		}
	}
}
