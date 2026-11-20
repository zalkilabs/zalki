package install

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const ukPath = "/home/user/.codex/config.toml"

// ukFragment builds the managed table the way the Codex adapter declares it.
func ukFragment(binary, state string) string {
	command, _ := json.Marshal(binary)
	args, _ := json.Marshal([]string{"--state", state, "mcp"})
	return "\n[mcp_servers.zalki]\ncommand = " + string(command) + "\nargs = " + string(args) + "\n"
}

func ukUpdate(before string, owned []string, next string) (string, error) {
	after, err := updateMCPFragment(ukPath, []byte(before), owned, next)
	return string(after), err
}

// The editor depends on go-toml's unstable parser giving the byte range of a
// whole key/value expression. These cases pin the exact offsets of the value
// range, so a go-toml change that moves them fails here and not for a user.
func TestTOMLValueRangeContract(t *testing.T) {
	tests := []struct{ name, doc, key, value string }{
		{"basic string", "[mcp_servers.zalki]\ncommand = \"x\"\n", "command", "\"x\""},
		{"literal string", "[mcp_servers.zalki]\ncommand = 'x'\n", "command", "'x'"},
		{"one-line array", "[mcp_servers.zalki]\nargs = [\"a\", \"b\"]\n", "args", "[\"a\", \"b\"]"},
		{"multi-line array", "[mcp_servers.zalki]\nargs = [\n  \"a\",\n  \"b\",\n]\ncommand = \"x\"\n", "args", "[\n  \"a\",\n  \"b\",\n]"},
		{"bracket in a string and a comment", "[mcp_servers.zalki]\nargs = [\n  \"a]\", # note ]\n  \"b\",\n]\ncommand = \"x\"\n", "args", "[\n  \"a]\", # note ]\n  \"b\",\n]"},
		{"trailing comment is excluded", "[mcp_servers.zalki]\nargs = [\"a\"] # note\n", "args", "[\"a\"]"},
		{"value at the end without a newline", "[mcp_servers.zalki]\ncommand = \"x\"", "command", "\"x\""},
		{"CRLF line ends", "[mcp_servers.zalki]\r\ncommand = \"x\"\r\nargs = [\"a\"]\r\n", "command", "\"x\""},
		{"CRLF line ends, array", "[mcp_servers.zalki]\r\ncommand = \"x\"\r\nargs = [\"a\"]\r\n", "args", "[\"a\"]"},
		{"quoted key and tabs", "[mcp_servers.zalki]\n\"command\"\t=\t\"x\"\n", "command", "\"x\""},
		{"spaces before the equals sign", "[mcp_servers.zalki]\ncommand   =   \"x\"\n", "command", "\"x\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if strings.Count(test.doc, test.value) != 1 {
				t.Fatal("test value is not unique in the document")
			}
			spans, found, err := locateZalkiTable([]byte(test.doc))
			if err != nil || !found {
				t.Fatal("table not located", found, err)
			}
			start := strings.Index(test.doc, test.value)
			got, ok := spans[test.key]
			if !ok || got.start != start || got.end != start+len(test.value) {
				t.Fatalf("range of %s = %+v, want [%d,%d)", test.key, got, start, start+len(test.value))
			}
		})
	}
}

func TestTOMLDottedKeysInsideTheTableAreTheUsers(t *testing.T) {
	doc := "[mcp_servers.zalki]\nenv.A = \"1\"\ncommand = \"x\"\n"
	spans, found, err := locateZalkiTable([]byte(doc))
	if err != nil || !found || len(spans) != 1 {
		t.Fatal("dotted key was taken as an owned key", spans, found, err)
	}
	if _, ok := spans["command"]; !ok {
		t.Fatal("command was not located")
	}
}

func TestTOMLLayoutsOtherThanOneExplicitHeaderAreRefused(t *testing.T) {
	refused := map[string]string{
		"inline table":             "[mcp_servers]\nzalki = { command = \"x\" }\n",
		"dotted key under servers": "[mcp_servers]\nzalki.command = \"x\"\n",
		"dotted key at the root":   "mcp_servers.zalki.command = \"x\"\n",
		"inline servers at root":   "mcp_servers = { zalki = { command = \"x\" } }\n",
		"array table":              "[[mcp_servers.zalki]]\ncommand = \"x\"\n",
		"two headers":              "[mcp_servers.zalki]\ncommand = \"x\"\n[mcp_servers.zalki]\nargs = []\n",
	}
	for name, doc := range refused {
		t.Run(name, func(t *testing.T) {
			if _, _, err := locateZalkiTable([]byte(doc)); !errors.Is(err, errZalkiLayout) {
				t.Fatalf("layout was not refused: %v", err)
			}
		})
	}
	accepted := map[string]string{
		"sub-table before the parent":   "[mcp_servers.zalki.env]\nA = \"1\"\n\n[mcp_servers.zalki]\ncommand = \"x\"\n",
		"sub-table after the parent":    "[mcp_servers.zalki]\ncommand = \"x\"\n\n[mcp_servers.zalki.tools.send_message]\napproval_mode = \"approve\"\n",
		"another server":                "[mcp_servers.other]\ncommand = \"y\"\n\n[mcp_servers.zalki]\ncommand = \"x\"\n",
		"header text in a comment":      "# [mcp_servers.zalki]\n[mcp_servers.zalki]\ncommand = \"x\"\n",
		"header text in a long string":  "note = \"\"\"\n[mcp_servers.zalki]\n\"\"\"\n[mcp_servers.zalki]\ncommand = \"x\"\n",
		"keys of an unrelated table":    "[mcp_servers.zalki]\ncommand = \"x\"\n\n[other]\ncommand = \"y\"\n",
		"a key named like the server":   "zalki = 1\n\n[mcp_servers.zalki]\ncommand = \"x\"\n",
		"dotted key of another server":  "mcp_servers.other.command = \"y\"\n\n[mcp_servers.zalki]\ncommand = \"x\"\n",
		"the managed table is the last": "[a]\nb = 1\n[mcp_servers.zalki]\ncommand = \"x\"",
	}
	for name, doc := range accepted {
		t.Run(name, func(t *testing.T) {
			spans, found, err := locateZalkiTable([]byte(doc))
			if err != nil || !found {
				t.Fatal("layout was refused", err)
			}
			if _, ok := spans["command"]; !ok || len(spans) != 1 {
				t.Fatal("wrong keys located", spans)
			}
		})
	}
}

func TestUpdateKeepsUserContentAndChangesOnlyOwnedValues(t *testing.T) {
	old, next := ukFragment("/old/zalki", "/s"), ukFragment("/new/zalki", "/s")
	// Includes a datetime, a float and an integer in an unrelated table: the
	// comparison outside the managed table keeps their types.
	before := "# my codex config\nmodel = \"gpt\"\n\n[mcp_servers.zalki.env]\nFOO = \"1\"\n\n[mcp_servers.zalki]\n# keep me\nstartup_timeout_sec = 30\n" +
		"command = \"/old/zalki\"   # trailing\nargs = [\n  \"--state\", \"/s\",   # multi\n  \"mcp\",\n]\ndefault_tools_approval_mode = \"approve\"\n\n" +
		"[mcp_servers.zalki.tools.send_message]\napproval_mode = \"approve\"\n\n[other]\nwhen = 1979-05-27T07:32:00Z\nratio = 1.0\nx = 1\n"
	want := strings.Replace(before, "\"/old/zalki\"", "\"/new/zalki\"", 1)
	got, err := ukUpdate(before, []string{old}, next)
	if err != nil || got != want {
		t.Fatalf("update changed more than the command value\n got %q\nwant %q\nerr %v", got, want, err)
	}
}

func TestUpdateWritesNextValueTextForChangedArgs(t *testing.T) {
	old, next := ukFragment("/old/zalki", "/s1"), ukFragment("/new/zalki", "/s2")
	before := "[other]\nx = 1\n" + old + "default_tools_approval_mode = \"approve\"\n"
	want := "[other]\nx = 1\n" + next + "default_tools_approval_mode = \"approve\"\n"
	got, err := ukUpdate(before, []string{old}, next)
	if err != nil || got != want {
		t.Fatalf("got %q want %q err %v", got, want, err)
	}
}

func TestUpdateWithUnchangedValuesWritesNothing(t *testing.T) {
	old := ukFragment("/old/zalki", "/s")
	before := "[other]\nx = 1\n" + old + "default_tools_approval_mode = \"approve\"\n"
	got, err := ukUpdate(before, []string{old}, old)
	if err != nil || got != before {
		t.Fatalf("an unchanged upgrade wrote bytes: %q %v", got, err)
	}
}

func TestUpdateRefusesAnEditedOrMissingOwnedKey(t *testing.T) {
	old, next := ukFragment("/old/zalki", "/s"), ukFragment("/new/zalki", "/s")
	edited := strings.Replace(old, "/old/zalki", "/hand/zalki", 1)
	deleted := "\n[mcp_servers.zalki]\nargs = [\"--state\", \"/s\", \"mcp\"]\n"
	for name, before := range map[string]string{"edited": edited, "deleted": deleted} {
		t.Run(name, func(t *testing.T) {
			_, err := ukUpdate(before, []string{old}, next)
			if err == nil || !strings.Contains(err.Error(), "key \"command\" was edited after Zalki wrote it") || !strings.Contains(err.Error(), ukPath) {
				t.Fatalf("refusal = %v", err)
			}
		})
	}
}

func TestUpdateHandlesAKeyTheUserAlreadySetToTheNextValue(t *testing.T) {
	old := ukFragment("/old/zalki", "/s")
	next := ukFragment("/new/zalki", "/s") + "startup_timeout_sec = 30\n"
	same := old + "startup_timeout_sec = 30\n"
	got, err := ukUpdate(same, []string{old}, next)
	if want := strings.Replace(same, "\"/old/zalki\"", "\"/new/zalki\"", 1); err != nil || got != want {
		t.Fatalf("same-value user key: got %q want %q err %v", got, want, err)
	}
	_, err = ukUpdate(old+"startup_timeout_sec = 60\n", []string{old}, next)
	if err == nil || !strings.Contains(err.Error(), "key \"startup_timeout_sec\" is already set by you to another value") {
		t.Fatalf("different-value user key: %v", err)
	}
	// Replace-only: a key that the file lacks would have to be added.
	_, err = ukUpdate(old, []string{old}, next)
	if err == nil || !strings.Contains(err.Error(), "cannot update the zalki table safely") {
		t.Fatalf("missing owned key: %v", err)
	}
}

func TestUpdateRefusesLayoutsItCannotEditInPlace(t *testing.T) {
	old, next := ukFragment("/old/zalki", "/s"), ukFragment("/new/zalki", "/s")
	layouts := []string{
		"[mcp_servers]\nzalki = { command = \"/old/zalki\", args = [\"--state\", \"/s\", \"mcp\"] }\n",
		"mcp_servers.zalki.command = \"/old/zalki\"\nmcp_servers.zalki.args = [\"--state\", \"/s\", \"mcp\"]\n",
		"[mcp_servers]\nzalki.command = \"/old/zalki\"\nzalki.args = [\"--state\", \"/s\", \"mcp\"]\n",
	}
	for _, before := range layouts {
		_, err := ukUpdate(before, []string{old}, next)
		if err == nil || !strings.Contains(err.Error(), "one explicit [mcp_servers.zalki] table") {
			t.Fatalf("layout %q: %v", before, err)
		}
	}
	// Other invalid or ambiguous forms are refused too.
	for _, before := range []string{old + old, "[[mcp_servers.zalki]]\ncommand = \"/old/zalki\"\n"} {
		if _, err := ukUpdate(before, []string{old}, next); err == nil {
			t.Fatalf("accepted %q", before)
		}
	}
}

func TestUpdateIgnoresHeaderTextInCommentsAndStrings(t *testing.T) {
	old, next := ukFragment("/old/zalki", "/s"), ukFragment("/new/zalki", "/s")
	before := "# [mcp_servers.zalki]\nnote = \"\"\"\n[mcp_servers.zalki]\ncommand = \"/old/zalki\"\n\"\"\"\n" + old
	// The same text sits inside the string; only the value in the real table changes.
	last := strings.LastIndex(before, "\"/old/zalki\"")
	want := before[:last] + "\"/new/zalki\"" + before[last+len("\"/old/zalki\""):]
	got, err := ukUpdate(before, []string{old}, next)
	if err != nil || got != want {
		t.Fatalf("got %q want %q err %v", got, want, err)
	}
}

func TestUpdateKeepsLineEndingsAndAMissingFinalNewline(t *testing.T) {
	old, next := ukFragment("/old/zalki", "/s"), ukFragment("/new/zalki", "/s")
	crlf := strings.ReplaceAll("[other]\nx = 1\n"+old+"default_tools_approval_mode = \"approve\"\n", "\n", "\r\n")
	got, err := ukUpdate(crlf, []string{old}, next)
	if want := strings.Replace(crlf, "\"/old/zalki\"", "\"/new/zalki\"", 1); err != nil || got != want {
		t.Fatalf("CRLF: got %q want %q err %v", got, want, err)
	}
	bare := strings.TrimSuffix(ukFragment("/old/zalki", "/s1"), "\n")
	got, err = ukUpdate(bare, []string{bare}, strings.TrimSuffix(ukFragment("/new/zalki", "/s2"), "\n"))
	if want := strings.TrimSuffix(ukFragment("/new/zalki", "/s2"), "\n"); err != nil || got != want {
		t.Fatalf("no final newline: got %q want %q err %v", got, want, err)
	}
}

// A byte order mark must either be kept or refused cleanly; it must never be damaged.
func TestUpdateNeverDamagesAFileWithAByteOrderMark(t *testing.T) {
	old, next := ukFragment("/old/zalki", "/s"), ukFragment("/new/zalki", "/s")
	before := "\xef\xbb\xbf" + old
	got, err := ukUpdate(before, []string{old}, next)
	if err == nil && got != "\xef\xbb\xbf"+next {
		t.Fatalf("BOM file was changed wrongly: %q", got)
	}
}

func TestUpdateAcrossReleasesAndBackReturnsTheSameBytes(t *testing.T) {
	a, b := ukFragment("/a/zalki", "/s"), ukFragment("/b/zalki", "/s")
	start := "[other]\nx = 1\n" + a + "default_tools_approval_mode = \"approve\"\n"
	middle, err := ukUpdate(start, []string{a}, b)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ukUpdate(middle, []string{b}, a)
	if err != nil || back != start || middle == start {
		t.Fatalf("A->B->A changed the file: %q %v", back, err)
	}
}

func TestUpdateWithoutTheTableAppendsIt(t *testing.T) {
	next := ukFragment("/new/zalki", "/s")
	got, err := ukUpdate("model = \"gpt\"\n", []string{ukFragment("/old/zalki", "/s")}, next)
	if err != nil || got != "model = \"gpt\"\n"+next {
		t.Fatalf("got %q err %v", got, err)
	}
}

// verifyZalkiEdit must refuse edits that damage the file even when the edited
// range looks right, so a range bug becomes a refusal and never a bad write.
func TestVerifyRefusesDamagedEdits(t *testing.T) {
	old, next := ukFragment("/old/zalki", "/s"), ukFragment("/new/zalki", "/s")
	before := old + "\n[other]\nratio = 1.0\n"
	current, err := mcpNamedTable([]byte(before), "zalki")
	if err != nil {
		t.Fatal(err)
	}
	_, wanted, err := mcpFragmentTable(next)
	if err != nil {
		t.Fatal(err)
	}
	plan := ownedPlan{set: []string{"command"}}
	spans, _, err := locateZalkiTable([]byte(before))
	if err != nil {
		t.Fatal(err)
	}
	command := spans["command"]
	good := []tomlEdit{{span: command, text: []byte("\"/new/zalki\"")}}
	if err := verifyZalkiEdit([]byte(before), splice([]byte(before), good), good, current, wanted, plan); err != nil {
		t.Fatalf("the intended edit was refused: %v", err)
	}
	// A byte changed outside the edited range.
	tampered := []byte(strings.Replace(string(splice([]byte(before), good)), "ratio = 1.0", "ratio = 2.0", 1))
	if err := verifyZalkiEdit([]byte(before), tampered, good, current, wanted, plan); !errors.Is(err, errZalkiUnsafe) {
		t.Fatalf("a changed byte outside the range was accepted: %v", err)
	}
	// An edit that injects a table and so moves args out of the managed table.
	inject := []tomlEdit{{span: command, text: []byte("\"/new/zalki\"\n[evil]\nx = 1")}}
	if err := verifyZalkiEdit([]byte(before), splice([]byte(before), inject), inject, current, wanted, plan); !errors.Is(err, errZalkiUnsafe) {
		t.Fatalf("an injected table was accepted: %v", err)
	}
	// A range that spills over an unrelated float and turns it into an integer.
	args := spans["args"]
	spill := []tomlEdit{{span: tomlSpan{start: command.start, end: strings.Index(before, "1.0") + len("1.0")}, text: []byte("\"/new/zalki\"\nargs = " + before[args.start:args.end] + "\n\n[other]\nratio = 1")}}
	if err := verifyZalkiEdit([]byte(before), splice([]byte(before), spill), spill, current, wanted, plan); !errors.Is(err, errZalkiUnsafe) {
		t.Fatalf("a float turned into an integer was accepted: %v", err)
	}
}
