package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func validStdioJSON(line []byte) bool {
	return utf8.Valid(line) && json.Valid(line)
}

type byteWriteCloser struct{ bytes.Buffer }

func (*byteWriteCloser) Close() error { return nil }

func TestStdioReaderRecoversOversizeMalformedAndCRLF(t *testing.T) {
	valid := "{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"ping\"}\r\n"
	for _, garbage := range []string{strings.Repeat("x", 140000) + "\n", "private-invalid-json\n", "bad\r\nbad-again\r\n"} {
		var errors bytes.Buffer
		reader := newStdioFrameReader(io.NopCloser(strings.NewReader(garbage+valid)), &errors)
		got, err := io.ReadAll(reader)
		if err != nil || string(got) != strings.TrimSuffix(valid, "\r\n")+"\n" {
			t.Fatalf("recovery lost next valid frame: %q %v", got, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(errors.String()), "\n") {
			if !validStdioJSON([]byte(line)) || !strings.Contains(line, `"id":null`) || strings.Contains(line, "private-invalid") {
				t.Fatalf("unsafe protocol error: %q", line)
			}
		}
	}
}

func TestStdioReaderNormalizesWhitespaceAfterCheckingWireLimit(t *testing.T) {
	const compact = `{"jsonrpc":"2.0","id":7,"method":"ping"}`
	oversized := compact + strings.Repeat(" ", maxStdioFrameBytes-len(compact)) + "\n"
	var output bytes.Buffer
	reader := newStdioFrameReader(io.NopCloser(strings.NewReader(oversized+compact+" \t\r\n")), &output)
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != compact+"\n" {
		t.Fatal("trailing whitespace was not normalized after wire-size validation", err)
	}
	if !strings.Contains(output.String(), `"id":7,"error":{"code":-32600`) {
		t.Fatal("an oversized frame became acceptable after trimming")
	}
}

func TestStdioReaderBoundsBufferAndUnterminatedDrain(t *testing.T) {
	var output bytes.Buffer
	reader := newStdioFrameReader(io.NopCloser(strings.NewReader(strings.Repeat("x", maxStdioDrainBytes+1))), &output)
	frame, oversized, err := reader.readFrame()
	if !errors.Is(err, errStdioDrainLimit) || !oversized || len(frame) != 0 {
		t.Fatalf("unbounded recovery was not terminated: bytes=%d oversized=%v err=%v", len(frame), oversized, err)
	}
	reader = newStdioFrameReader(io.NopCloser(strings.NewReader(strings.Repeat("x", 140000)+"\n")), &output)
	frame, oversized, err = reader.readFrame()
	if err != nil || !oversized || len(frame) > maxStdioFrameBytes {
		t.Fatalf("oversized tail retained: bytes=%d oversized=%v err=%v", len(frame), oversized, err)
	}
}

func TestStdioReaderStopsAtHardLimitWithOpenInput(t *testing.T) {
	input, writer := io.Pipe()
	t.Cleanup(func() { _ = input.Close(); _ = writer.Close() })
	reader := newStdioFrameReader(input, io.Discard)
	done := make(chan error, 1)
	go func() {
		_, _, err := reader.readFrame()
		done <- err
	}()
	go func() {
		_, _ = io.WriteString(writer, strings.Repeat("x", maxStdioDrainBytes+1))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errStdioDrainLimit) {
			t.Fatal("open input returned the wrong drain error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drain kept waiting after the first byte over the hard limit")
	}
}

type fragmentReader struct{ input string }

func (r *fragmentReader) Read(p []byte) (int, error) {
	if r.input == "" {
		return 0, io.EOF
	}
	p[0] = r.input[0]
	r.input = r.input[1:]
	return 1, nil
}

func TestStdioReaderAcceptsFragmentedAndExactBoundaryFrames(t *testing.T) {
	// Derive the exact boundary independently of the object field lengths.
	base := `{"x":"` + strings.Repeat("x", maxStdioFrameBytes-len("{\"x\":\"\"}\n")) + "\"}\n"
	var output bytes.Buffer
	reader := newStdioFrameReader(io.NopCloser(&fragmentReader{input: base}), &output)
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != base || output.Len() != 0 || len(got) != maxStdioFrameBytes {
		t.Fatalf("boundary changed: bytes=%d err=%v", len(got), err)
	}
	reader = newStdioFrameReader(io.NopCloser(&fragmentReader{input: "{bad\n{\"x\":1}"}), &output)
	got, err = io.ReadAll(reader)
	if err != nil || string(got) != "{\"x\":1}\n" || output.String() != malformedFrameReply {
		t.Fatalf("fragmented recovery/EOF changed: %q %v", got, err)
	}
}

type partialWriter struct{ byteWriteCloser }

func (w *partialWriter) Write(p []byte) (int, error) {
	return w.Buffer.Write(p[:1])
}

func TestStdioWriterSerializesShortWritesAndProtocolErrors(t *testing.T) {
	var dest partialWriter
	writer := &stdioFrameWriter{output: &dest}
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			if _, err := io.WriteString(writer, malformedFrameReply); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if dest.String() != strings.Repeat(malformedFrameReply, 20) {
		t.Fatal("concurrent short writes interleaved frame bytes")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("writer accepted bytes after close", err)
	}
}

type blockedWriter struct {
	started chan struct{}
	closed  chan struct{}
}

func (w *blockedWriter) Write([]byte) (int, error) {
	close(w.started)
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *blockedWriter) Close() error { close(w.closed); return nil }

func TestStdioWriterCloseInterruptsBlockedOutput(t *testing.T) {
	dest := &blockedWriter{started: make(chan struct{}), closed: make(chan struct{})}
	writer := &stdioFrameWriter{output: dest}
	done := make(chan struct{})
	go func() { _, _ = writer.Write([]byte("frame")); close(done) }()
	<-dest.started
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closing the connection waited behind a blocked write")
	}
}

func TestStdioOversizeIDRecoveryIsTopLevelAndPrefixBounded(t *testing.T) {
	digits := strings.Repeat("7", maxStdioIDPrefix-32)
	for _, test := range []struct{ name, prefix, want string }{
		{"number first", `{"id":17,"method":"tools/call","params":"unfinished`, "17"},
		{"negative", `{"id":-17}`, "-17"},
		{"fraction", `{"id":0.125}`, "0.125"},
		{"exponent", `{"id":-1.25e+8}`, "-1.25e+8"},
		{"exponent outside float64", `{"id":1e10000}`, "1e10000"},
		{"long number", `{"id":` + digits + `}`, digits},
		{"long digit string", `{"id":"` + digits + `"}`, `"` + digits + `"`},
		{"number beyond prefix", `{"id":` + strings.Repeat("7", maxStdioIDPrefix) + `}`, ""},
		{"string beyond prefix", `{"id":"` + strings.Repeat("7", maxStdioIDPrefix) + `"}`, ""},
		{"string first", `{"id":"call-a","method":"tools/call","params":"unfinished`, `"call-a"`},
		{"invalid UTF-8 ID", "{\"id\":\"bad\xff\"}", ""},
		{"raw NUL ID", "{\"id\":\"bad\x00\"}", ""},
		{"escaped control ID", `{"id":"a\u0000b"}`, `"a\u0000b"`},
		{"escaped quote ID", `{"id":"a\"b"}`, `"a\"b"`},
		{"after fields", `{"params":{"id":"nested"},"method":"tools/call","id":"right","padding":"unfinished`, `"right"`},
		{"nested only", `{"method":"tools/call","params":{"id":99},"padding":"unfinished`, ""},
		{"after prefix", `{"padding":"` + strings.Repeat("x", maxStdioIDPrefix) + `","id":7}`, ""},
		{"duplicate", `{"id":1,"id":2,"padding":"unfinished`, ""},
		{"true ID", `{"id":true,"method":"tools/call"}`, ""},
		{"false ID", `{"id":false,"method":"tools/call"}`, ""},
		{"null ID", `{"id":null,"method":"tools/call"}`, ""},
		{"object ID", `{"id":{"n":1}}`, ""},
		{"array ID", `{"id":[1]}`, ""},
		{"cut number", `{"id":123`, ""},
		{"malformed prefix", `{"id":1,bad}`, ""},
		{"wrong close", `{"id":1]`, ""},
		{"trailing junk", `{"id":1}x`, ""},
		{"second object", `{"id":1} {"id":2}`, ""},
		{"complete with whitespace", "{\"id\":1} \t\r\n", "1"},
		{"incomplete after complete ID", `{"id":1 `, "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := string(recoverFrameID([]byte(test.prefix))); got != test.want {
				t.Fatalf("recovered ID %q, want %q", got, test.want)
			}
		})
	}
}

func TestStdioOversizeErrorCorrelatesButMalformedJSONDoesNot(t *testing.T) {
	oversize := `{"id":"call","method":"tools/call","params":{"body":"` + strings.Repeat("x", 140000) + "\"}}\n"
	var output bytes.Buffer
	reader := newStdioFrameReader(io.NopCloser(strings.NewReader(oversize+"{\"id\":8,bad}\n{\"x\":1}\n")), &output)
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != "{\"x\":1}\n" {
		t.Fatal("recovery failed", err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"id":"call"`) || lines[1]+"\n" != malformedFrameReply {
		t.Fatalf("wrong correlated/fallback errors: %q", lines)
	}
}
