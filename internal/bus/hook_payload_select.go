package bus

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

const (
	maxExtendedHookWire = 32 * 1024 * 1024
	maxHookDepth        = 10000 // encoding/json's existing nesting ceiling
)

// Only trusted adapter opt-in uses this selector. The complete first object is
// validated before the ordinary typed decoder or any lifecycle operation. Bulk
// strings/containers never become tokens, RawMessages, or retained byte slices.
func decodeSelectedHookPayload(ctx context.Context, input io.Reader, event *agent.HookInput, extendedEvent string) (int, []byte, error) {
	reader := &hookSelectorInput{Reader: input, ctx: ctx}
	s := hookPayloadSelector{input: bufio.NewReader(io.LimitReader(reader, maxExtendedHookWire+1))}
	s.object()
	if s.err != nil {
		return s.wire, append([]byte(nil), s.kept[:min(len(s.kept), hookHeaderLimit)]...), s.err
	}
	_, header, err := decodeHookPayload(bytes.NewReader(s.kept), event)
	if s.wire > maxHookPayload && event.Event != extendedEvent {
		err = model.NewError(model.CodeInvalidInput, "hook payload exceeds 8 MiB")
	}
	return s.wire, header, err
}

// Preserve ordinary small-hook cancellation/validation ordering. Only reads in
// the new extended allowance check cancellation. As with the existing decoder,
// an arbitrary Reader already blocked inside Read cannot be interrupted here.
type hookSelectorInput struct {
	io.Reader
	ctx  context.Context
	read int
}

func (r *hookSelectorInput) Read(p []byte) (int, error) {
	if r.read >= maxHookPayload {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
	} else {
		p = p[:min(len(p), maxHookPayload-r.read)]
	}
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

type hookPayloadSelector struct {
	input   *bufio.Reader
	kept    []byte
	wire    int
	started bool
	err     error
}

func (s *hookPayloadSelector) invalid() {
	if s.err == nil {
		s.err = model.NewError(model.CodeInvalidInput, "invalid hook object")
	}
}

func (s *hookPayloadSelector) peek() byte {
	if s.err != nil {
		return 0
	}
	b, err := s.input.Peek(1)
	if err != nil {
		if errors.Is(err, io.EOF) && s.started {
			err = io.ErrUnexpectedEOF
		}
		s.err = err
		return 0
	}
	return b[0]
}

func (s *hookPayloadSelector) emit(b byte) {
	if s.err != nil {
		return
	}
	if len(s.kept) == maxHookPayload {
		s.err = model.NewError(model.CodeInvalidInput, "hook metadata exceeds 8 MiB")
		return
	}
	s.kept = append(s.kept, b)
}

func (s *hookPayloadSelector) take(keep bool) byte {
	b := s.peek()
	if s.err != nil {
		return 0
	}
	_, s.err = s.input.ReadByte()
	s.wire++
	if s.wire > maxExtendedHookWire {
		s.err = model.NewError(model.CodeInvalidInput, "hook payload exceeds 32 MiB")
	}
	if keep {
		s.emit(b)
	}
	return b
}

func (s *hookPayloadSelector) expect(want byte, keep bool) {
	if s.take(keep) != want {
		s.invalid()
	}
}

func (s *hookPayloadSelector) space(keep bool) {
	for s.err == nil && strings.ContainsRune(" \t\r\n", rune(s.peek())) {
		s.take(keep)
	}
}

// Keys are unescaped before duplicate checks, including case aliases that the
// typed decoder could otherwise interpret as the same lifecycle/identity key.
// Only exact top-level bulk keys are dropped; nested lookalikes remain data.
func (s *hookPayloadSelector) object() {
	s.space(false)
	s.expect('{', false)
	s.started = true
	s.emit('{')
	seen := map[string]bool{}
	fields := 0
	s.space(false)
	if s.peek() == '}' {
		s.take(true)
		return
	}
	for s.err == nil {
		start := len(s.kept)
		if fields > 0 {
			s.emit(',')
		}
		keyStart := len(s.kept)
		s.text(true)
		if s.err != nil {
			return
		}
		var key string
		if json.Unmarshal(s.kept[keyStart:], &key) != nil {
			s.invalid()
			return
		}
		folded := strings.ToLower(strings.ToUpper(key))
		if seen[folded] {
			s.invalid()
			return
		}
		seen[folded] = true
		keep := key != "tool_input" && key != "tool_response"
		if !keep {
			s.kept = s.kept[:start]
		}
		s.space(false)
		s.expect(':', false)
		if keep {
			s.emit(':')
			fields++
		}
		s.space(false)
		s.value(keep, 1)
		s.space(false)
		if s.peek() == '}' {
			s.take(true)
			return
		}
		s.expect(',', false)
		s.space(false)
	}
}

func (s *hookPayloadSelector) value(keep bool, depth int) {
	b := s.peek()
	switch {
	case b == '"':
		s.text(keep)
	case b == '{' || b == '[':
		if depth == maxHookDepth {
			s.invalid()
			return
		}
		s.take(keep)
		s.members(keep, depth+1, b == '{')
	case b == 't':
		s.literal("true", keep)
	case b == 'f':
		s.literal("false", keep)
	case b == 'n':
		s.literal("null", keep)
	case b == '-' || (b >= '0' && b <= '9'):
		s.number(keep)
	default:
		s.invalid()
	}
}

func (s *hookPayloadSelector) members(keep bool, depth int, object bool) {
	end := byte(']')
	if object {
		end = '}'
	}
	s.space(keep)
	if s.peek() == end {
		s.take(keep)
		return
	}
	for s.err == nil {
		if object {
			s.text(keep)
			s.space(keep)
			s.expect(':', keep)
			s.space(keep)
		}
		s.value(keep, depth)
		s.space(keep)
		if s.peek() == end {
			s.take(keep)
			return
		}
		s.expect(',', keep)
		s.space(keep)
	}
}

func (s *hookPayloadSelector) text(keep bool) {
	s.expect('"', keep)
	for s.err == nil {
		b := s.take(keep)
		switch {
		case b == '"':
			return
		case b == '\\':
			escaped := s.take(keep)
			if escaped == 'u' {
				for range 4 {
					h := s.take(keep)
					if (h < '0' || h > '9') && (h < 'a' || h > 'f') && (h < 'A' || h > 'F') {
						s.invalid()
					}
				}
			} else if !strings.ContainsRune(`"\/bfnrt`, rune(escaped)) {
				s.invalid()
			}
		case b < 0x20:
			s.invalid()
		}
	}
}

func (s *hookPayloadSelector) literal(word string, keep bool) {
	for i := range len(word) {
		s.expect(word[i], keep)
	}
}

func (s *hookPayloadSelector) digits(keep bool) {
	if b := s.peek(); b < '0' || b > '9' {
		s.invalid()
		return
	}
	for s.err == nil {
		b := s.peek()
		if b < '0' || b > '9' {
			return
		}
		s.take(keep)
	}
}

func (s *hookPayloadSelector) number(keep bool) {
	if s.peek() == '-' {
		s.take(keep)
	}
	if s.peek() == '0' {
		s.take(keep)
	} else {
		s.digits(keep)
	}
	if s.peek() == '.' {
		s.take(keep)
		s.digits(keep)
	}
	if b := s.peek(); b == 'e' || b == 'E' {
		s.take(keep)
		if sign := s.peek(); sign == '+' || sign == '-' {
			s.take(keep)
		}
		s.digits(keep)
	}
}
