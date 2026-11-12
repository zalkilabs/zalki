// Package plugins runs the single explicitly enabled local reference plugin.
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

// ChildCommand is the private reference-child subcommand of the same binary.
const ChildCommand = "plugin-notifier"

const maxFrame = 64 << 10

// helloTimeout bounds how long run and check wait for a plugin's first frame.
// Only the plugin's own installation lease is held during the wait, never the
// Store lock, and cancellation still stops the child at once. A cold first
// launch (a new unsigned binary, an interpreter under load) can take seconds,
// so a plugin that never says hello fails after this deadline, not sooner.
const helloTimeout = 10 * time.Second

var errPartialFrameEOF = errors.New("plugin stream ended mid-frame")

// errHelloVersion refuses any hello other than feed v1, v1-read or v2.
var errHelloVersion = errors.New("plugin requires hello.version=v1, v1-read or v2; rebuild a v0 plugin against feed v1")

type hello struct {
	Version string `json:"version"`
}

type frame struct {
	Hello *hello       `json:"hello,omitempty"`
	ID    string       `json:"id,omitempty"`
	Gap   string       `json:"gap,omitempty"`
	Event *model.Event `json:"event,omitempty"`
	Ack   string       `json:"ack,omitempty"`
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

func writeFrame(output io.Writer, value frame) error {
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

func (f frame) validHello() bool {
	return f.Hello != nil && f.Hello.Version == "v1" && f.ID == "" && f.Ack == "" && f.Gap == "" && f.Event == nil
}
