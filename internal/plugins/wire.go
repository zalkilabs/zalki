// Package plugins runs the single explicitly enabled local reference plugin.
package plugins

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/zalkilabs/zalki/internal/model"
)

// ChildCommand is the private reference-child subcommand of the same binary.
const ChildCommand = "plugin-notifier"

const maxFrame = 64 << 10

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
	if err != nil {
		if len(raw) != 0 {
			return value, io.ErrUnexpectedEOF
		}
		return value, err
	}
	if len(raw) > maxFrame {
		return value, errors.New("plugin frame exceeds 64KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		if errors.Is(err, io.EOF) {
			return value, io.ErrUnexpectedEOF
		}
		return value, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return value, errors.New("plugin frame must contain one JSON value")
	}
	return value, nil
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
	return f.Hello != nil && f.Hello.Version == "v0" && f.ID == "" && f.Ack == "" && f.Gap == "" && f.Event == nil
}
