package bus

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

const (
	maxHookPayload  = 8 * 1024 * 1024
	hookHeaderLimit = 16 * 1024
)

// Decode incrementally, without waiting for EOF after the first value. The
// extra byte distinguishes an oversized value from malformed JSON at the cap.
func decodeHookPayload(input io.Reader, event *agent.HookInput) (int, []byte, error) {
	reader := io.LimitedReader{R: input, N: maxHookPayload + 1}
	decoder := json.NewDecoder(&reader)
	err := decoder.Decode(event)
	size := maxHookPayload + 1 - int(reader.N)
	offset := decoder.InputOffset()
	if offset > maxHookPayload || (err != nil && offset == 0 && size > maxHookPayload) {
		err = model.NewError(model.CodeInvalidInput, "hook payload exceeds 8 MiB")
	}
	var header []byte
	if err != nil && offset == 0 {
		// Buffered is memory-only. After a consumed value it contains trailing input,
		// which must never be mistaken for the failed value's header.
		header, _ = io.ReadAll(io.LimitReader(decoder.Buffered(), hookHeaderLimit))
	}
	return size, header, err
}

func hookPayloadSizeClass(size int) string {
	switch {
	case size <= 128*1024:
		return "le_128kib"
	case size <= maxHookPayload:
		return "128kib_to_8mib"
	default:
		return "gt_8mib"
	}
}

// Probe the bounded top-level header without treating nested fields or strings
// as headers. Events beyond the prefix are unknown after a syntax/size failure.
func probeHookEvent(data []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ""
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return ""
		}
		if key == "hook_event_name" {
			var event string
			if decoder.Decode(&event) != nil {
				return ""
			}
			return event
		}
		var ignored json.RawMessage
		if decoder.Decode(&ignored) != nil {
			return ""
		}
	}
	return ""
}
