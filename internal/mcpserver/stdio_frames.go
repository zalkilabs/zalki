package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

const (
	maxStdioFrameBytes = 128 * 1024
	maxStdioIDPrefix   = 16 * 1024
	// Sixty-four ordinary frame budgets bound recovery work without buffering the discarded tail.
	maxStdioDrainBytes  = 8 * 1024 * 1024
	oversizeFrameReply  = "{\"jsonrpc\":\"2.0\",\"id\":null,\"error\":{\"code\":-32600,\"message\":\"request too large; limit 128 KiB\"}}\n"
	malformedFrameReply = "{\"jsonrpc\":\"2.0\",\"id\":null,\"error\":{\"code\":-32700,\"message\":\"Parse error\"}}\n"
)

var errStdioDrainLimit = errors.New("inbound JSON-RPC frame exceeded 8 MiB recovery drain limit")

type stdioFrameReader struct {
	input   io.ReadCloser
	reader  *bufio.Reader
	output  io.Writer
	pending []byte
	prefix  []byte
}

func newStdioFrameReader(input io.ReadCloser, output io.Writer) *stdioFrameReader {
	return &stdioFrameReader{input: input, reader: bufio.NewReaderSize(input, maxStdioFrameBytes), output: output}
}

func (r *stdioFrameReader) Close() error { return r.input.Close() }

func (r *stdioFrameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		frame, err := r.acceptFrame()
		if err != nil {
			return 0, err
		}
		r.pending = frame
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *stdioFrameReader) acceptFrame() ([]byte, error) {
	for {
		frame, oversized, err := r.readFrame()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(frame) == 0 && !oversized && errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if !oversized && (len(frame) == 0 || frame[len(frame)-1] != '\n') {
			frame = append(frame, '\n')
			oversized = len(frame) > maxStdioFrameBytes
		}
		failure := r.frameFailure(frame, oversized)
		if failure == "" {
			// The SDK expects a delimiter immediately after the decoded value.
			return append(bytes.TrimRight(frame, " \t\r\n"), '\n'), nil
		}
		if _, writeErr := io.WriteString(r.output, failure); writeErr != nil {
			return nil, writeErr
		}
	}
}

func (r *stdioFrameReader) frameFailure(frame []byte, oversized bool) string {
	if oversized {
		id := recoverFrameID(r.prefix)
		if len(id) == 0 {
			return oversizeFrameReply
		}
		return `{"jsonrpc":"2.0","id":` + string(id) + ",\"error\":{\"code\":-32600,\"message\":\"request too large; limit 128 KiB\"}}\n"
	}
	if !json.Valid(frame) {
		return malformedFrameReply
	}
	return ""
}

func (r *stdioFrameReader) readFrame() ([]byte, bool, error) {
	r.prefix = r.prefix[:0]
	total := 0
	for {
		part, err := r.reader.ReadSlice('\n')
		total += len(part)
		if remaining := maxStdioIDPrefix - len(r.prefix); remaining > 0 {
			r.prefix = append(r.prefix, part[:min(remaining, len(part))]...)
		}
		if total > maxStdioDrainBytes {
			return nil, true, errStdioDrainLimit
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if total > len(part) || total > maxStdioFrameBytes {
				return nil, true, err
			}
			return part, false, err
		}
		if total == maxStdioDrainBytes {
			// After the drain budget, wait for one byte or EOF instead of another line fragment.
			_, err := r.reader.ReadByte()
			if err == nil {
				err = errStdioDrainLimit
			}
			return nil, true, err
		}
	}
}

// Scan top-level fields only. The prefix may end inside an otherwise valid object.
func recoverFrameID(prefix []byte) json.RawMessage {
	prefix = prefix[:min(len(prefix), maxStdioIDPrefix)]
	decoder := json.NewDecoder(bytes.NewReader(prefix))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil
	}
	var id json.RawMessage
	seen := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil || key == "id" && seen {
			return nil
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			if key != "id" && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
				return id
			}
			return nil
		}
		if key == "id" {
			seen = true
			if !completeFrameID(value, decoder.InputOffset() == int64(len(prefix))) {
				return nil
			}
			id = value
		}
	}
	return finishFrameID(decoder, prefix, id)
}

func finishFrameID(decoder *json.Decoder, prefix []byte, id json.RawMessage) json.RawMessage {
	end, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return id
	}
	if err != nil || end != json.Delim('}') || len(bytes.Trim(prefix[decoder.InputOffset():], " \t\r\n")) != 0 {
		return nil
	}
	return id
}

func completeFrameID(value json.RawMessage, atEnd bool) bool {
	if len(value) == 0 {
		return false
	}
	if value[0] == '"' {
		// RawMessage preserves the original bytes, including invalid UTF-8.
		return utf8.Valid(value)
	}
	// A number cut at the prefix boundary may have more digits.
	return !atEnd && (value[0] == '-' || value[0] >= '0' && value[0] <= '9')
}

// All SDK responses and recovery errors share this lock, including short underlying writes.
type stdioFrameWriter struct {
	mu        sync.Mutex
	output    io.WriteCloser
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (w *stdioFrameWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	written := 0
	for written < len(p) {
		n, err := w.output.Write(p[written:])
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (w *stdioFrameWriter) Close() error {
	w.closeOnce.Do(func() {
		w.closed.Store(true)
		w.closeErr = w.output.Close()
	})
	return w.closeErr
}
