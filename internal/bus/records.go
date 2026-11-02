package bus

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const recordSchema = 1

// leaseSessionSchema marks a session record that carries a listener lease.
// Older builds drop unknown fields when they rewrite a record, so a lease must
// never be stored under a schema they accept. Records without one stay at
// recordSchema and remain readable by older builds.
const leaseSessionSchema = 2

// Legacy records have schema zero. Reads migrate in memory; writes persist the
// current schema. An older binary must never overwrite a future record format.
func readRecord(path string, value any, schema *int) error {
	if err := readJSON(path, value); err != nil {
		return err
	}
	if *schema < 0 || *schema > recordSchema {
		return fail(CodeUnsupportedSchema, "unsupported journal schema %d", *schema)
	}
	return nil
}

func (s Store) readSession(path string) (Session, error) {
	var raw json.RawMessage
	var v Session
	if err := readJSON(path, &raw); err != nil {
		return v, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	if v.Schema < 0 || v.Schema > leaseSessionSchema {
		return v, fail(CodeUnsupportedSchema, "unsupported session schema %d", v.Schema)
	}
	if v.Listener != nil && v.Schema != leaseSessionSchema {
		return v, fail(CodeUnsupportedSchema, "a listener lease requires session schema %d", leaseSessionSchema)
	}
	if v.NativeID == "" {
		v.NativeID = NativeID(v)
	}
	if expected, err := s.sessionID(v.Kind, v.NativeID); err != nil || expected != v.ID {
		return v, fail(CodeInvalidIdentity, "native and Zalki session identities do not match")
	}
	extensions, err := stateExtensions(raw)
	if err != nil {
		return v, err
	}
	v.AdapterState, err = s.decodeAdapterState(v.Kind, StateInput{Extensions: extensions, State: v.AdapterState})
	return v, err
}

func canonicalNativeID(id string) (string, error) {
	if len(id) == 0 || len(id) > 1024 || !utf8.ValidString(id) || strings.ContainsFunc(id, unicode.IsControl) {
		return "", fail(CodeInvalidIdentity, "native session ID must be 1..1024 UTF-8 bytes without control characters")
	}
	if uuid, err := UUID(id); err == nil {
		return uuid, nil
	}
	return id, nil
}

// Length-prefixed components prevent ambiguity even when native IDs contain
// separators. UUID-shaped native IDs keep their original public address.
func derivedID(namespace string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(namespace))
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "\x00%d:%s", len(part), part) // hash.Hash writes never fail
	}
	b := h.Sum(nil)
	b[6], b[8] = b[6]&15|128, b[8]&63|128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s Store) rawSessions() ([]Session, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "sessions", "*.json"))
	if err != nil {
		return nil, err
	}
	items := []Session{}
	for _, path := range files {
		v, err := s.readSession(path)
		if err != nil {
			return nil, err
		}
		// Validate identities before using records as paths in cleanup.
		expected, err := s.sessionPath(v.ID)
		if err != nil || expected != path || !strings.HasPrefix(v.ID, v.Kind+":") {
			return nil, errors.New("session file identity mismatch")
		}
		items = append(items, v)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}
