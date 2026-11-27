package antigravity

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The two replies are the shapes agy 1.5.18 accepted in a live run: a system
// message injected before a model call, and the decision that continues a turn.
// The bytes are fixed on purpose.

const replyFixture = "[zalki message M]\nFrom: A \u00b7 \"b\"\nBody <x> & y"

func requireGoldenBytes(t *testing.T, name string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("differs from testdata/%s\ngot:  %q\nwant: %q", name, got, want)
	}
}

func TestInjectStepsReplyIsExactlyTheGoldenFile(t *testing.T) {
	var out bytes.Buffer
	if err := writeReply(&out, injectStepsReply(replyFixture)); err != nil {
		t.Fatal(err)
	}
	requireGoldenBytes(t, "inject-steps-reply.jsonl", out.Bytes())
}

func TestContinueReplyIsExactlyTheGoldenFile(t *testing.T) {
	var out bytes.Buffer
	if err := writeReply(&out, continueReply(replyFixture)); err != nil {
		t.Fatal(err)
	}
	requireGoldenBytes(t, "continue-reply.jsonl", out.Bytes())
}
