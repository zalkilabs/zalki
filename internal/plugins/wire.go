// Package plugins runs explicitly enabled local plugins over the one plugin wire.
package plugins

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

const maxFrame = 64 << 10

// helloTimeout bounds how long run and check wait for a plugin's first frame.
// Only the plugin's own installation lease is held during the wait, never the
// Store lock, and cancellation still stops the child at once. A cold first
// launch (a new unsigned binary, an interpreter under load) can take seconds,
// so a plugin that never says hello fails after this deadline, not sooner.
const helloTimeout = 10 * time.Second

var errPartialFrameEOF = errors.New("plugin stream ended mid-frame")

// wireName is the one plugin wire this host speaks.
const wireName = "zalki-plugin/1"

// errHelloVersion refuses any hello the one plugin wire does not speak.
var errHelloVersion = errors.New("plugin must start with the zalki-plugin/1 hello; rebuild it against the current plugin wire")

// hello is the plugin's hello. Version is decoded only to name an old wire
// (v1, v1-read, v2) in the refusal; the current wire never sets it.
type hello struct {
	Wire     string   `json:"wire,omitempty"`
	Version  string   `json:"version,omitempty"`
	Requires []string `json:"requires,omitempty"`
	Optional []string `json:"optional,omitempty"`
}

// frame is one plugin frame: the hello, an ack, or one request in the
// envelope {op, request_id, idempotency_key, args}. The host also writes
// feed offers ({id, gap, event}) with it.
type frame struct {
	Hello          *hello          `json:"hello,omitempty"`
	ID             string          `json:"id,omitempty"`
	Gap            string          `json:"gap,omitempty"`
	Event          *model.Event    `json:"event,omitempty"`
	Ack            string          `json:"ack,omitempty"`
	Op             string          `json:"op,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Args           json.RawMessage `json:"args,omitempty"`
}

// isRequest reports a frame in the request envelope.
func (f frame) isRequest() bool {
	return f.Op != "" || f.RequestID != "" || f.IdempotencyKey != "" || f.Args != nil
}

func readFrame(reader *bufio.Reader) (frame, error) {
	var value frame
	raw, err := reader.ReadSlice('\n')
	if len(raw) > maxFrame {
		return value, errors.New("plugin frame exceeds 64KiB")
	}
	if err != nil {
		if len(raw) == 0 {
			return value, err
		}
		if errors.Is(err, io.EOF) {
			return value, errors.Join(io.ErrUnexpectedEOF, errPartialFrameEOF)
		}
		return value, errors.Join(io.ErrUnexpectedEOF, err)
	}
	err = decodeFrame(raw, &value)
	return value, err
}

func decodeFrame(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("plugin frame must contain one JSON value")
	}
	return nil
}

func writeFrame(output io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > maxFrame {
		return errors.New("plugin frame exceeds 64KiB")
	}
	n, err := output.Write(raw)
	if err == nil && n != len(raw) {
		return io.ErrShortWrite
	}
	return err
}
