package pluginfeed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

var errStreamWork = errors.New("bounded plugin feed work is unavailable")

type streamBudget struct {
	mu        sync.Mutex
	ctx       context.Context
	now       func() time.Time
	deadline  time.Time
	used      [5]int
	eventRead func(bus.JournalView, string, uint64) (model.Event, error)
}

func newStreamBudget(ctx context.Context) *streamBudget {
	now := time.Now()
	return &streamBudget{ctx: ctx, now: time.Now, deadline: now.Add(50 * time.Millisecond)}
}

func (b *streamBudget) charge(input, reads, native, tokens, probes int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if !b.now().Before(b.deadline) {
		return errStreamWork
	}
	if !streamBudgetFits(input, b.used[0], 4<<20) || !streamBudgetFits(reads, b.used[1], 640) || !streamBudgetFits(native, b.used[2], 512) || !streamBudgetFits(tokens, b.used[3], 262144) || !streamBudgetFits(probes, b.used[4], 512) {
		return errStreamWork
	}
	b.used[0] += input
	b.used[1] += reads
	b.used[2] += native
	b.used[3] += tokens
	b.used[4] += probes
	return nil
}

func streamBudgetFits(amount, used, limit int) bool { return amount >= 0 && amount <= limit-used }

func (b *streamBudget) read(path string, limit int) ([]byte, error) {
	if err := b.charge(0, 0, 0, 0, 0); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, journal.ErrInvalidFile
	}
	if err := b.charge(int(info.Size()), 1, 0, 0, 0); err != nil {
		return nil, err
	}
	raw, err := journal.ReadBytes(path, int(info.Size()))
	if err != nil {
		return nil, err
	}
	if err := b.tokens(raw); err != nil {
		return nil, err
	}
	return raw, b.charge(0, 0, 0, 0, 0)
}

func (b *streamBudget) tokens(raw []byte) error {
	if !utf8.Valid(raw) {
		return journal.ErrInvalidFile
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	depth := 0
	for {
		if err := b.charge(0, 0, 0, 1, 0); err != nil {
			return err
		}
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			if depth < 0 || depth > 64 {
				return journal.ErrInvalidFile
			}
		}
	}
}
