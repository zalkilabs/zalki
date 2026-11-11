package plugins

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWireRefusesBadFrames(t *testing.T) {
	for _, raw := range []string{"", "{} {}\n", "{\"unknown\":1}\n", "{\"ack\":\"x\"}", strings.Repeat("x", maxFrame+2) + "\n"} {
		if _, err := readFrame(bufio.NewReaderSize(strings.NewReader(raw), maxFrame+1)); err == nil {
			t.Fatal("accepted invalid frame", len(raw))
		}
	}
	var output bytes.Buffer
	if err := writeFrame(&output, frame{Hello: &hello{Version: "v1"}}); err != nil {
		t.Fatal(err)
	}
	value, err := readFrame(bufio.NewReaderSize(&output, maxFrame+1))
	if err != nil || value.Hello == nil || value.Hello.Version != "v1" {
		t.Fatal("hello roundtrip", value, err)
	}
	if err := writeFrame(shortWriter{}, frame{Ack: "00000000-0000-4000-8000-000000000001"}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short write was hidden", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestWireDistinguishesEmptyFrameFromStreamEOF(t *testing.T) {
	for name, raw := range map[string]string{"empty_line": "\n", "whitespace_line": "  \t\n"} {
		t.Run(name, func(t *testing.T) {
			_, err := readFrame(bufio.NewReaderSize(strings.NewReader(raw), maxFrame+1))
			if err == nil || errors.Is(err, io.EOF) {
				t.Fatal("complete empty frame was accepted as clean EOF", err)
			}
		})
	}
	if _, err := readFrame(bufio.NewReaderSize(strings.NewReader(""), maxFrame+1)); !errors.Is(err, io.EOF) {
		t.Fatal("clean stream EOF was not preserved", err)
	}
}

func TestWireEnforcesFrameSizeBoundaries(t *testing.T) {
	const body = `{"hello":{"version":"v1"}}`
	for _, size := range []int{maxFrame, maxFrame + 1} {
		raw := body + strings.Repeat(" ", size-len(body)-1) + "\n"
		value, err := readFrame(bufio.NewReaderSize(strings.NewReader(raw), maxFrame+1))
		if size == maxFrame && (err != nil || !value.validHello()) {
			t.Fatal("exact-limit frame was refused", err)
		}
		if size > maxFrame && err == nil {
			t.Fatal("otherwise-valid oversized frame was accepted")
		}
	}
	var output bytes.Buffer
	if err := writeFrame(&output, frame{Ack: strings.Repeat("x", maxFrame)}); err == nil || output.Len() != 0 {
		t.Fatal("oversized output frame was written", err, output.Len())
	}
}

// Feed v1 is a clean break: a v0 hello is refused.
func TestHelloRequiresFeedV1(t *testing.T) {
	for version, want := range map[string]bool{"v1": true, "v0": false, "v1-read": false, "": false} {
		if got := (frame{Hello: &hello{Version: version}}).validHello(); got != want {
			t.Fatalf("validHello(%q) = %v, want %v", version, got, want)
		}
	}
	if !strings.Contains(errHelloVersion.Error(), "v1") || !strings.Contains(errHelloVersion.Error(), "v0") {
		t.Fatalf("hello error does not name the versions: %v", errHelloVersion)
	}
}
