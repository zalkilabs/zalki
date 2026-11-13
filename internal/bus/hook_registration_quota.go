package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

const promptQuotaFileLimit = 64 * 1024
const promptQuotaWindow = 60 * time.Second
const promptQuotaCount = 20

var errPromptQuota = errors.New("prompt registration quota exceeded")
var errPromptQuotaState = errors.New("unsafe prompt registration quota")

type promptQuotaAttempt struct {
	Actor string `json:"actor"`
	At    string `json:"at"`
}

type promptQuota struct {
	Schema   int                  `json:"schema"`
	Attempts []promptQuotaAttempt `json:"attempts"`
}

// The caller holds Store.locked, after WAL recovery. A failed synced write
// denies creation even if the rename published the debit. Denied attempts do
// not extend buckets. No credential, native ID, cwd or prompt enters this file.
func debitPromptRegistration(dir, actor string, now time.Time, write func(string, []byte) error) error {
	if model.ValidateSessionAddress(actor) != nil || now.IsZero() {
		return errPromptQuotaState
	}
	path := filepath.Join(dir, "hook-registration-quota.json")
	quota, err := readPromptQuota(path)
	if err != nil {
		return err
	}
	live, err := livePromptQuota(quota, now)
	if err != nil {
		return err
	}
	for _, attempt := range live {
		if attempt.Actor == actor {
			return errPromptQuota
		}
	}
	if len(live) >= promptQuotaCount {
		return errPromptQuota
	}
	live = append(live, promptQuotaAttempt{Actor: actor, At: now.UTC().Format(time.RFC3339Nano)})
	raw, err := journal.EncodeJSON(promptQuota{Schema: 1, Attempts: live}, promptQuotaFileLimit)
	if err != nil {
		return errPromptQuotaState
	}
	return write(path, raw)
}

func readPromptQuota(path string) (promptQuota, error) {
	if err := checkPromptQuotaFile(path); err != nil {
		return promptQuota{}, err
	}
	raw, err := journal.ReadBytes(path, promptQuotaFileLimit)
	if errors.Is(err, os.ErrNotExist) {
		return promptQuota{Schema: 1, Attempts: []promptQuotaAttempt{}}, nil
	}
	if err != nil {
		return promptQuota{}, errPromptQuotaState
	}
	fields, err := promptQuotaFields(raw, "schema", "attempts")
	var quota promptQuota
	if err != nil || json.Unmarshal(fields["schema"], &quota.Schema) != nil || quota.Schema != 1 {
		return quota, errPromptQuotaState
	}
	attempts := bytes.TrimSpace(fields["attempts"])
	if len(attempts) == 0 || attempts[0] != '[' {
		return quota, errPromptQuotaState
	}
	var rows []json.RawMessage
	if json.Unmarshal(attempts, &rows) != nil || len(rows) > promptQuotaCount {
		return quota, errPromptQuotaState
	}
	quota.Attempts = make([]promptQuotaAttempt, 0, len(rows))
	for _, row := range rows {
		attempt, err := decodePromptAttempt(row)
		if err != nil {
			return quota, err
		}
		quota.Attempts = append(quota.Attempts, attempt)
	}
	return quota, nil
}

func decodePromptAttempt(raw []byte) (promptQuotaAttempt, error) {
	var attempt promptQuotaAttempt
	fields, err := promptQuotaFields(raw, "actor", "at")
	if err != nil || json.Unmarshal(fields["actor"], &attempt.Actor) != nil || json.Unmarshal(fields["at"], &attempt.At) != nil {
		return attempt, errPromptQuotaState
	}
	return attempt, nil
}

// Validate the complete ledger before expiry can hide corrupt or ambiguous
// entries. UTC canonical strings and unique canonical actors are mandatory.
func livePromptQuota(quota promptQuota, now time.Time) ([]promptQuotaAttempt, error) {
	if quota.Schema != 1 || len(quota.Attempts) > promptQuotaCount {
		return nil, errPromptQuotaState
	}
	live := make([]promptQuotaAttempt, 0, len(quota.Attempts))
	seen := map[string]bool{}
	for _, attempt := range quota.Attempts {
		at, err := time.Parse(time.RFC3339Nano, attempt.At)
		if err != nil || at.IsZero() || at.UTC().Format(time.RFC3339Nano) != attempt.At || model.ValidateSessionAddress(attempt.Actor) != nil || seen[attempt.Actor] || now.Before(at) {
			return nil, errPromptQuotaState
		}
		seen[attempt.Actor] = true
		if now.Sub(at) < promptQuotaWindow {
			live = append(live, attempt)
		}
	}
	return live, nil
}

// JSON object decoding with exact fields and duplicate rejection. It also
// catches alternate escaped spellings of the same field. Values stay raw until
// their expected types are checked; malformed UTF-8 is never repaired.
func promptQuotaFields(raw []byte, first, second string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, errPromptQuotaState
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errPromptQuotaState
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || (key != first && key != second) || fields[key] != nil {
			return nil, errPromptQuotaState
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errPromptQuotaState
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || len(fields) != 2 {
		return nil, errPromptQuotaState
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errPromptQuotaState
	}
	return fields, nil
}

func checkPromptQuotaFile(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !privatePromptQuotaFile(info, os.Getuid()) {
			return errPromptQuotaState
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errPromptQuotaState
	}
	return nil
}

func privatePromptQuotaFile(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0 && int64(stat.Uid) == int64(uid)
}
