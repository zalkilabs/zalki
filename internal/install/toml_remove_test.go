package install

import (
	"errors"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// Uninstall removes the managed Codex table with the user's keys and sub-tables.
// The block finder depends on go-toml's unstable parser: a table header has no
// byte range of its own, and a comment is not an expression. These tests pin the
// ranges, so a go-toml change that moves them fails here and not for a user.

func TestZalkiBlockRangeContract(t *testing.T) {
	tests := []struct {
		name, doc string
		want      []string
	}{
		{"plain", "[a]\nk = 1\n[mcp_servers.zalki]\ncommand = \"c\"\n[b]\nk = 2\n", []string{"[mcp_servers.zalki]\ncommand = \"c\"\n"}},
		{"empty line before goes too", "[a]\nk = 1\n\n[mcp_servers.zalki]\ncommand = \"c\"\n", []string{"\n[mcp_servers.zalki]\ncommand = \"c\"\n"}},
		{"a line with spaces stays", "[a]\n \n[mcp_servers.zalki]\ncommand = \"c\"\n", []string{"[mcp_servers.zalki]\ncommand = \"c\"\n"}},
		{"indented header", "[a]\n   [mcp_servers.zalki]\ncommand = \"c\"\n", []string{"   [mcp_servers.zalki]\ncommand = \"c\"\n"}},
		{"spaces inside the brackets", "[ mcp_servers . zalki ]\ncommand = \"c\"\n", []string{"[ mcp_servers . zalki ]\ncommand = \"c\"\n"}},
		{"quoted key", "[a]\n[mcp_servers.\"zalki\"]\ncommand = \"c\"\n", []string{"[mcp_servers.\"zalki\"]\ncommand = \"c\"\n"}},
		{"comments on the header and key lines", "[mcp_servers.zalki] # h\ncommand = \"c\" # k\n[b]\n", []string{"[mcp_servers.zalki] # h\ncommand = \"c\" # k\n"}},
		{"comment inside goes, comment after stays", "[mcp_servers.zalki]\n# in\ncommand = \"c\"\n# after\n[b]\n", []string{"[mcp_servers.zalki]\n# in\ncommand = \"c\"\n"}},
		{"header with no keys", "[mcp_servers.zalki] # h\n[b]\nk = 1\n", []string{"[mcp_servers.zalki] # h\n"}},
		{"header at the end with no newline", "[a]\nk = 1\n[mcp_servers.zalki]", []string{"[mcp_servers.zalki]"}},
		{"last key at the end with no newline", "[mcp_servers.zalki]\ncommand = \"c\"", []string{"[mcp_servers.zalki]\ncommand = \"c\""}},
		{"crlf", "[a]\r\n\r\n[mcp_servers.zalki]\r\ncommand = \"c\"\r\n[b]\r\n", []string{"\r\n[mcp_servers.zalki]\r\ncommand = \"c\"\r\n"}},
		{"multi-line value", "[mcp_servers.zalki]\nn = \"\"\"\n[b]\n\"\"\"\n[b]\n", []string{"[mcp_servers.zalki]\nn = \"\"\"\n[b]\n\"\"\"\n"}},
		{"sub-tables and array tables", "[mcp_servers.zalki.env]\nA = \"1\"\n\n[x]\n\n[mcp_servers.zalki]\ncommand = \"c\"\n\n[[mcp_servers.zalki.hooks]]\nn = 1\n",
			[]string{"[mcp_servers.zalki.env]\nA = \"1\"\n", "\n[mcp_servers.zalki]\ncommand = \"c\"\n", "\n[[mcp_servers.zalki.hooks]]\nn = 1\n"}},
		{"another server is not a block", "[mcp_servers.other]\ncommand = \"o\"\n[mcp_servers.zalki]\ncommand = \"c\"\n", []string{"[mcp_servers.zalki]\ncommand = \"c\"\n"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			blocks, err := zalkiBlocks([]byte(test.doc))
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(blocks))
			for _, block := range blocks {
				got = append(got, test.doc[block.start:block.end])
			}
			if strings.Join(got, "|") != strings.Join(test.want, "|") || len(got) != len(test.want) {
				t.Fatalf("blocks\n got %q\nwant %q", got, test.want)
			}
		})
	}
}

func TestRemoveZalkiTableKeepsEverythingElse(t *testing.T) {
	tests := []struct{ name, before, after string }{
		{"the table is the whole file", "\n[mcp_servers.zalki]\ncommand = \"c\"\nargs = [\"a\"]\nextra = 1\n", ""},
		{"between two tables with a blank line each side", "[a]\nk = 1\n\n[mcp_servers.zalki]\ncommand = \"c\"\nx = 1\n\n[b]\nk = 2\n", "[a]\nk = 1\n\n[b]\nk = 2\n"},
		{"between two tables with no blank line", "[a]\nk = 1\n[mcp_servers.zalki]\ncommand = \"c\"\nx = 1\n[b]\nk = 2\n", "[a]\nk = 1\n[b]\nk = 2\n"},
		{"last in the file", "model = \"m\"\n\n[mcp_servers.zalki]\ncommand = \"c\"\nx = 1\n", "model = \"m\"\n"},
		{"sub-table before the parent", "[mcp_servers.zalki.env]\nA = \"1\"\n\n[mcp_servers.zalki]\ncommand = \"c\"\n\n[other]\nk = 1\n", "\n[other]\nk = 1\n"},
		{"sub-table after the parent with a table between", "[mcp_servers.zalki]\ncommand = \"c\"\n\n[other]\nk = 1\n\n[mcp_servers.zalki.env]\nA = \"1\"\n", "\n[other]\nk = 1\n"},
		{"blocks before and after another table", "[mcp_servers.zalki.env]\nA = \"1\"\n[x]\nk = 1\n[mcp_servers.zalki]\ncommand = \"c\"\n", "[x]\nk = 1\n"},
		{"array tables under the entry", "[mcp_servers.zalki]\ncommand = \"c\"\n\n[[mcp_servers.zalki.hooks]]\nn = 1\n\n[[mcp_servers.zalki.hooks]]\nn = 2\n[tail]\nz = 1\n", "[tail]\nz = 1\n"},
		{"crlf", "[a]\r\nk = 1\r\n\r\n[mcp_servers.zalki]\r\ncommand = \"c\"\r\nx = 1\r\n\r\n[b]\r\nk = 2\r\n", "[a]\r\nk = 1\r\n\r\n[b]\r\nk = 2\r\n"},
		{"mixed line endings", "[a]\nk = 1\r\n\n[mcp_servers.zalki]\r\ncommand = \"c\"\nx = 1\r\n[b]\nk = 2\n", "[a]\nk = 1\r\n[b]\nk = 2\n"},
		{"a line with spaces before the header stays", "[a]\nk = 1\n  \n[mcp_servers.zalki]\ncommand = \"c\"\nx = 1\n", "[a]\nk = 1\n  \n"},
		{"no final newline", "[a]\nk = 1\n\n[mcp_servers.zalki]\ncommand = \"c\"\nx = 1", "[a]\nk = 1\n"},
		{"indented header with a comment and a quoted key", "[a]\nk=1\n  [ mcp_servers . \"zalki\" ] # note\ncommand = \"c\" # c1\nx = 1\n", "[a]\nk=1\n"},
		{"header with sub-table only", "[mcp_servers.zalki]\n[mcp_servers.zalki.env]\nA = \"1\"\n", ""},
		{"comment inside goes, comment after stays", "[mcp_servers.zalki]\n# inside\ncommand = \"c\"\nx = 1\n# next\n[b]\nk = 2\n", "# next\n[b]\nk = 2\n"},
		{"a multi-line value with header text", "[mcp_servers.zalki]\ncommand = \"c\"\nnote = \"\"\"\n[b]\n\"\"\"\n[b]\nk = 2\n", "[b]\nk = 2\n"},
		{"comments and other servers stay", "# mine\n[mcp_servers.other]\ncommand = \"o\"\n\n# zalki\n[mcp_servers.zalki]\ncommand = \"c\"\nx = 1\n", "# mine\n[mcp_servers.other]\ncommand = \"o\"\n\n# zalki\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := removeZalkiTable([]byte(test.before))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != test.after {
				t.Fatalf("result\n got %q\nwant %q", got, test.after)
			}
			var doc map[string]any
			if err := toml.Unmarshal(got, &doc); err != nil {
				t.Fatal("the result does not parse", err)
			}
			if servers, _ := doc["mcp_servers"].(map[string]any); servers["zalki"] != nil {
				t.Fatal("the zalki table is still there")
			}
		})
	}
}

func TestRemoveZalkiTableRefusesLayoutsItCannotRemove(t *testing.T) {
	layouts := map[string]string{
		"inline table":             "[mcp_servers]\nzalki = { command = \"x\", env = { A = \"1\" } }\n",
		"dotted key under servers": "[mcp_servers]\nzalki.command = \"x\"\n",
		"dotted key at the root":   "mcp_servers.zalki.command = \"x\"\n",
		"array table":              "[[mcp_servers.zalki]]\ncommand = \"x\"\n",
		"two headers":              "[mcp_servers.zalki]\ncommand = \"x\"\n[mcp_servers.zalki]\nargs = []\n",
	}
	for name, doc := range layouts {
		t.Run(name, func(t *testing.T) {
			if _, err := removeZalkiTable([]byte(doc)); err == nil || (!errors.Is(err, errZalkiRemoveLayout) && !errors.Is(err, errZalkiRemoveUnsafe)) {
				t.Fatal("layout was not refused with an uninstall message", err)
			}
		})
	}
	if _, err := removeZalkiTable([]byte("[other]\nk = 1\n")); !errors.Is(err, errZalkiRemoveUnsafe) {
		t.Fatal("a file with no zalki table was not refused", err)
	}
	for _, text := range []error{errZalkiRemoveLayout, errZalkiRemoveUnsafe} {
		if !strings.Contains(text.Error(), "run zalki uninstall again") {
			t.Fatal("the refusal does not say to run uninstall again", text)
		}
	}
}

func TestRemoveZalkiTableNeverDamagesAFileWithAByteOrderMark(t *testing.T) {
	doc := "\xef\xbb\xbf[a]\nk = 1\n\n[mcp_servers.zalki]\ncommand = \"c\"\nx = 1\n"
	got, err := removeZalkiTable([]byte(doc))
	if err != nil {
		return // refusing is safe
	}
	if !strings.HasPrefix(string(got), "\xef\xbb\xbf[a]\nk = 1\n") || strings.Contains(string(got), "zalki") {
		t.Fatalf("a file with a byte order mark was damaged: %q", got)
	}
}

func TestVerifyZalkiRemovalRefusesDamagedResults(t *testing.T) {
	before := "[a]\nk = 1\n\n[mcp_servers.zalki]\ncommand = \"c\"\nx = 1\n\n[b]\nk = 2\n"
	blocks, err := zalkiBlocks([]byte(before))
	if err != nil || len(blocks) != 1 {
		t.Fatal("blocks", blocks, err)
	}
	edits := []tomlEdit{{span: blocks[0]}}
	good := string(splice([]byte(before), edits))
	if err := verifyZalkiRemoval([]byte(before), []byte(good), edits); err != nil {
		t.Fatal("the right result was refused", err)
	}
	damaged := map[string]string{
		"extra text":               good + "# extra\n",
		"another table changed":    strings.Replace(good, "k = 2", "k = 3", 1),
		"the zalki table is there": before,
		"a value outside changed":  strings.Replace(good, "k = 1", "k = 9", 1),
		"does not parse":           good + "[",
	}
	for name, after := range damaged {
		t.Run(name, func(t *testing.T) {
			if err := verifyZalkiRemoval([]byte(before), []byte(after), edits); !errors.Is(err, errZalkiRemoveUnsafe) {
				t.Fatal("a damaged result was accepted", err)
			}
		})
	}
}

func TestUserKeyNamesAreSafeAndBounded(t *testing.T) {
	long := strings.Repeat("k", 100)
	tests := []struct{ key, want string }{
		{"env", "env"},
		{"default_tools_approval_mode", "default_tools_approval_mode"},
		{"a.b-c_D1", "a.b-c_D1"},
		{"", `""`},
		{"has space", `"has space"`},
		{"line\nbreak", `"line\nbreak"`},
		{"esc\x1b[31m", `"esc\x1b[31m"`},
		{"bidi\u202eevil", "\"bidi\\u202eevil\""},
		{"zero\u200bwidth", "\"zero\\u200bwidth\""},
		{"üñí", `"üñí"`},
		{long, `"` + long[:maxUserKeyName] + `"...`},
	}
	for _, test := range tests {
		if got := userKeyName(test.key); got != test.want {
			t.Errorf("userKeyName(%q) = %s, want %s", test.key, got, test.want)
		}
	}
}

func TestUserKeysNoteTextAndCap(t *testing.T) {
	keys := make([]string, 25)
	for i := range keys {
		keys[i] = "k" + string(rune('a'+i))
	}
	notes := userKeysNotes("/h/.claude.json", "mcpServers.zalki", "entry", keys)
	if len(notes) != 1 || len(userKeysNotes("/h/.claude.json", "mcpServers.zalki", "entry", nil)) != 0 {
		t.Fatal("expected one note for keys and none without keys", notes)
	}
	preview, result := notes[0].preview, notes[0].result
	for _, want := range []string{"/h/.claude.json: mcpServers.zalki has keys you added (ka, kb,", ", and 5 more)", "Uninstall would remove the whole entry with them.", "Values are not shown.", "A backup named /h/.claude.json.zalki-backup-* will keep the old file."} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview lacks %q: %s", want, preview)
		}
	}
	for _, want := range []string{"mcpServers.zalki had keys you added", "Uninstall removed the whole entry with them.", "The backup /h/.claude.json.zalki-backup-* keeps the old file."} {
		if !strings.Contains(result, want) {
			t.Errorf("result lacks %q: %s", want, result)
		}
	}
	if strings.Contains(preview, "ky") { // the 25th name (index 24) is "ky": only the first 20 are listed
		t.Error("the note lists more than the cap")
	}
}
