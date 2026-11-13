package pluginfeed

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// A new installation is announced at most startNoticeLimit times, globally
// per Store, and only within startNoticeWindow after it was installed.
const (
	startNoticeLimit  = 3
	startNoticeWindow = 24 * time.Hour
	startNoticeBytes  = 4 << 10
)

// startNoticeLedger counts the SessionStart outputs that announced one
// installation. It is not a grant, a receipt, a policy or part of the
// revision; it lives in plugins/NAME.start-notice.json (0600).
type startNoticeLedger struct {
	Installation string             `json:"installation"`
	Starts       []startNoticeToken `json:"starts"`
}

// startNoticeToken is one SessionStart: the same actor and generation is one
// token, a new generation is a new one.
type startNoticeToken struct {
	Actor      string `json:"actor"`
	Generation string `json:"generation"`
}

var errStartNoticeLedger = errors.New("plugin start-notice ledger is invalid or belongs to another installation")

func (s pluginFiles) startNoticePath() string {
	return filepath.Join(s.pluginDir(), s.name+".start-notice.json")
}

// startNoticeDue reports whether the record may still be announced at now:
// a current-shape receipt (proven, terminal or unproven installer) whose
// installed_at is not in the future and less than 24 hours ago. A record
// without a receipt has unknown provenance and is never announced.
func startNoticeDue(p pluginRecord, now time.Time) bool {
	if p.Install == nil || !p.Install.valid() {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, p.Install.InstalledAt)
	return err == nil && !at.After(now) && now.Before(at.Add(startNoticeWindow))
}

// loadStartNotice reads the ledger of installation p.ID. A missing file is an
// empty ledger for this installation. A file of another installation, or one
// that is unsafe or malformed, is an error and is never reset or adopted.
func (s pluginFiles) loadStartNotice(p pluginRecord) (startNoticeLedger, error) {
	empty := startNoticeLedger{Installation: p.ID, Starts: []startNoticeToken{}}
	raw, err := journal.ReadBytes(s.startNoticePath(), startNoticeBytes)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	var ledger startNoticeLedger
	if bus.DecodeStrictRecordJSON(raw, &ledger) != nil || !ledger.valid(p.ID) {
		return empty, errStartNoticeLedger
	}
	return ledger, nil
}

func (l startNoticeLedger) valid(installation string) bool {
	if l.Installation != installation || l.Starts == nil || len(l.Starts) > startNoticeLimit {
		return false
	}
	for i, token := range l.Starts {
		if model.ValidateSessionAddress(token.Actor) != nil || model.IsPluginInboxAddress(token.Actor) || !model.IsCanonicalUUID(token.Generation) || slices.Contains(l.Starts[:i], token) {
			return false
		}
	}
	return true
}

// reserveStartNotice spends one announcement of the current installation for
// this SessionStart (actor, generation), durably, before the line is shown.
// It returns false, with nothing written, when the installation is not due,
// the budget of 3 is spent, or this start already holds a token (a repeated
// start never spends twice). The caller holds the Store lock and has already
// checked that the whole output fits; it shows the line only on true.
func (s pluginFiles) reserveStartNotice(p pluginRecord, actor, generation string, now time.Time) (bool, error) {
	if !startNoticeDue(p, now) {
		return false, nil
	}
	ledger, err := s.loadStartNotice(p)
	if err != nil {
		return false, err
	}
	token := startNoticeToken{Actor: actor, Generation: generation}
	if slices.Contains(ledger.Starts, token) || len(ledger.Starts) >= startNoticeLimit {
		return false, nil
	}
	ledger.Starts = append(ledger.Starts, token)
	if !ledger.valid(p.ID) {
		return false, errStartNoticeLedger
	}
	raw, err := journal.EncodeJSON(ledger, startNoticeBytes)
	if err != nil {
		return false, err
	}
	// A failed or uncertain write withholds the line (false); the count may
	// still be published, and the caller never refunds it.
	if err := journal.WriteAtomic(s.startNoticePath(), raw); err != nil {
		return false, err
	}
	return true, nil
}
