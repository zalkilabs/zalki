package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/journal"
)

func (w *pluginViewWork) check(input, reads, native, tokens, probes int) error {
	if w == nil || w.charge == nil {
		return journal.ErrInvalidFile
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	return w.charge(input, reads, native, tokens, probes)
}

func readScopedPluginBytes(store Store, path string, limit int) ([]byte, error) {
	if value, ok := pluginMemberViews.Load(store.dir); ok {
		if work := value.(*pluginMemberCache).work.Load(); work != nil {
			return work.read(path, limit)
		}
	}
	return journal.ReadBytes(path, limit)
}

func (w *pluginViewWork) read(path string, limit int) ([]byte, error) {
	if err := w.check(0, 0, 0, 0, 0); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, journal.ErrInvalidFile
	}
	// Reserve size before allocation; using that exact limit also rejects growth
	// after stat. A shorter read conservatively keeps the reserved debit.
	if err := w.check(int(info.Size()), 1, 0, 0, 0); err != nil {
		return nil, err
	}
	raw, err := journal.ReadBytes(path, int(info.Size()))
	if err != nil {
		return nil, err
	}
	if err := w.tokens(raw); err != nil {
		return nil, err
	}
	return raw, w.check(0, 0, 0, 0, 0)
}

func (w *pluginViewWork) tokens(raw []byte) error {
	if !utf8.Valid(raw) {
		return journal.ErrInvalidFile
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	depth := 0
	for {
		if err := w.check(0, 0, 0, 1, 0); err != nil {
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
