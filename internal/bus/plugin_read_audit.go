package bus

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
)

type pluginReadAuditRow struct {
	At       time.Time `json:"at"`
	Plugin   string    `json:"plugin"`
	Request  string    `json:"request"`
	Session  string    `json:"session"`
	Message  string    `json:"message"`
	Revision string    `json:"revision"`
	Outcome  string    `json:"outcome"`
	Bytes    int       `json:"response_bytes"`
}

type pluginReadAudit struct {
	Version int                  `json:"version"`
	Rows    []pluginReadAuditRow `json:"rows"`
}

func (s PluginFeed) loadPluginReadAudit(now time.Time) ([]pluginReadAuditRow, error) {
	return s.loadPluginReadAuditFile("plugin-read-audit.json", now, func(row pluginReadAuditRow) error {
		return s.checkCapabilityGrants(PluginCapabilityGrants{Read: []string{row.Session}})
	}, false)
}

func (s PluginFeed) loadPluginReadAuditFile(name string, now time.Time, validate func(pluginReadAuditRow) error, requireRows bool) ([]pluginReadAuditRow, error) {
	raw, err := journal.ReadBytes(filepath.Join(s.store.Dir, name), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var log pluginReadAudit
	if err := decodeEventJSON(raw, &log); err != nil {
		return nil, err
	}
	if log.Version != 1 || len(log.Rows) > 1000 || (requireRows && log.Rows == nil) {
		return nil, journal.ErrInvalidFile
	}
	rows := make([]pluginReadAuditRow, 0, len(log.Rows))
	for i := range log.Rows {
		row := &log.Rows[i]
		if !validPluginReadAuditRow(row) {
			return nil, journal.ErrInvalidFile
		}
		if err := validate(*row); err != nil {
			return nil, err
		}
		if !row.At.Before(now.Add(-30 * 24 * time.Hour)) {
			rows = append(rows, *row)
		}
	}
	return rows, nil
}

func (s PluginFeed) savePluginReadAudit(rows []pluginReadAuditRow, now time.Time) error {
	return s.savePluginReadAuditFile("plugin-read-audit.json", rows, now)
}

func (s PluginFeed) savePluginReadAuditFile(name string, rows []pluginReadAuditRow, now time.Time) error {
	for {
		if len(rows) > 1000 {
			if now.Sub(rows[0].At) < time.Second {
				return journal.ErrInvalidFile
			}
			rows = rows[1:]
			continue
		}
		raw, err := journal.EncodeJSON(pluginReadAudit{Version: 1, Rows: rows}, 1<<20)
		if err == nil {
			return journal.WriteAtomic(filepath.Join(s.store.Dir, name), raw)
		}
		var size *journal.SizeError
		if !errors.As(err, &size) || len(rows) < 2 || now.Sub(rows[0].At) < time.Second {
			return err
		}
		rows = rows[1:]
	}
}

// Audit outcomes describe authorization, not proof that stdout was delivered.
func validPluginReadAuditRow(row *pluginReadAuditRow) bool {
	return !row.At.IsZero() && canonicalEventID(row.Plugin) && canonicalEventID(row.Request) && canonicalEventID(row.Message) && canonicalEventID(row.Revision) && row.Bytes >= 0 && row.Bytes <= 32<<10 && slices.Contains([]string{"ok", "native_deferred", "denied", "revoked", "stale_binding", "stale_target", "unavailable", "too_large"}, row.Outcome)
}
