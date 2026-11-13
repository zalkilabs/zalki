package pluginfeed

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

var errPluginStartupRecords = errors.New("plugin startup records are unavailable")
var errPluginStartupNotice = errors.New("plugin startup notice reservation is unavailable")
var errPluginStartupFit = errors.New("plugin startup output does not fit")

// SessionPluginStartup composes bounded native hook text under the caller's
// live write scope. Optional notices are fully sized BEFORE their durable debit.
func SessionPluginStartup(view bus.JournalView, root string, session model.Session, reserve bool, fits func(string) (bool, error)) (string, error) {
	var text string
	err := view.WithWriteScope(root, func() error {
		records, err := sessionStartupRecords(root)
		if err != nil {
			return err
		}
		text = sessionStartupSummary(view, session, records)
		if fits != nil {
			ok, err := fits(text)
			if err != nil || !ok {
				return errPluginStartupFit
			}
		} else if reserve {
			return errPluginStartupFit
		}
		if !reserve {
			return nil
		}
		var noticeErr error
		text, noticeErr = appendSessionStartNotices(root, session, records, text, fits)
		return noticeErr
	})
	return text, err
}

func sessionStartupRecords(root string) ([]pluginRecord, error) {
	dir := filepath.Join(root, "plugins")
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if pluginDirectory(dir, false) != nil {
		return nil, errPluginStartupRecords
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errPluginStartupRecords
	}
	records := []pluginRecord{}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		if name == entry.Name() || len(name) > 32 || !model.ValidKind(name) {
			continue
		}
		if len(records) >= 512 {
			return nil, errPluginStartupRecords
		}
		p, _, err := (pluginFiles{root: root, name: name}).loadPlugin()
		if errors.Is(err, ErrOldPluginRecord) {
			continue
		}
		if err != nil {
			return nil, errPluginStartupRecords
		}
		records = append(records, p)
	}
	return records, nil
}

func sessionStartupSummary(view bus.JournalView, session model.Session, records []pluginRecord) string {
	var policy struct {
		Mode    string `json:"mode"`
		Invalid bool   `json:"invalid_env"`
	}
	if len(session.PluginPolicy) == 0 || json.Unmarshal(session.PluginPolicy, &policy) != nil {
		return "Plugins active for this session: none (session plugin policy unavailable)."
	}
	names := []string{}
	for _, p := range records {
		if sessionStartupEligible(view, session.ID, p) {
			names = append(names, p.Name)
		}
	}
	slices.Sort(names)
	line := "Plugins active for this session: none."
	if policy.Mode == "off" {
		line = "Plugins active for this session: none (ZALKI_PLUGINS=off)."
	}
	if len(names) > 0 {
		line = "Plugins active for this session: " + strings.Join(names[:min(len(names), 16)], ", ") + "."
	}
	if len(names) > 16 {
		line = "Plugins active for this session: " + strings.Join(names[:16], ", ") + fmt.Sprintf(", +%d more. See: zalki plugins list", len(names)-16)
	}
	if policy.Invalid {
		line += "\nInvalid ZALKI_PLUGINS; plugins are off for this session. Use off or comma-separated plugin names."
	}
	return line
}

func sessionStartupEligible(view bus.JournalView, session string, p pluginRecord) bool {
	permitted := p.Scope == scopeAll && (p.Access.Status || p.Access.Receive) || slices.Contains(p.Access.Send, session) || slices.Contains(p.Access.Read, session)
	if !p.Enabled || !permitted {
		return false
	}
	member, err := view.PluginMember(session, p.Name)
	return err == nil && member
}

func appendSessionStartNotices(root string, session model.Session, records []pluginRecord, text string, fits func(string) (bool, error)) (string, error) {
	return appendSessionStartNoticesWith(records, text, fits, func(p pluginRecord, now time.Time) (bool, error) {
		return (pluginFiles{root: root, name: p.Name}).reserveStartNotice(p, session.ID, session.Generation, now)
	})
}

func appendSessionStartNoticesWith(records []pluginRecord, text string, fits func(string) (bool, error), reserve func(pluginRecord, time.Time) (bool, error)) (string, error) {
	slices.SortFunc(records, func(a, b pluginRecord) int {
		if order := strings.Compare(installTime(a), installTime(b)); order != 0 {
			return order
		}
		return strings.Compare(a.Name, b.Name)
	})
	count := 0
	var first error
	now := time.Now().UTC()
	for _, p := range records {
		if count == 3 {
			break
		}
		if !startNoticeDue(p, now) {
			continue
		}
		line := sessionInstallNotice(p)
		candidate := text + "\n" + line
		ok, err := fits(candidate)
		if err != nil || !ok {
			continue
		}
		allowed, err := reserve(p, now)
		if err != nil {
			first = errPluginStartupNotice
			continue
		}
		if allowed {
			text = candidate
			count++
		}
	}
	return text, first
}

func installTime(p pluginRecord) string {
	if p.Install == nil {
		return ""
	}
	return p.Install.InstalledAt
}

func sessionInstallNotice(p pluginRecord) string {
	at, _ := time.Parse(time.RFC3339Nano, p.Install.InstalledAt)
	installer := "an unverified installer"
	switch p.Install.InstalledBy {
	case "unproven":
	case "terminal":
		installer = "terminal"
	default:
		label := p.Install.InstalledByLabel
		if label == "" {
			label = p.Install.InstalledBy
		}
		installer = "session " + label
	}
	return fmt.Sprintf("New plugin: %s, installed %s UTC by %s. See: zalki plugins list", p.Name, at.UTC().Format("15:04"), installer)
}
