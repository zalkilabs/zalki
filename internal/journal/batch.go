package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

const (
	maxBatchBytes = 1 << 20
	maxBatchPuts  = 32
	maxPutBytes   = 128 << 10
)

// ErrInvalidBatch identifies a malformed, unsupported or unsafe redo intent.
var ErrInvalidBatch = errors.New("invalid journal batch")

// ErrBatchConflict identifies a pending intent or target with different bytes.
var ErrBatchConflict = errors.New("journal batch conflict")

// Put replaces one relative path with exact bytes. PreviousSHA256 is empty
// only when the previous file must be absent. SHA256 protects the after-image.
type Put struct {
	// Path is a clean relative file path below the caller-owned root.
	Path string `json:"path"`
	// PreviousSHA256 is the expected prior content digest, or empty for absent.
	PreviousSHA256 string `json:"previous_sha256,omitempty"`
	// Data is the exact after-image, encoded as base64 in an intent.
	Data []byte `json:"data"`
	// SHA256 is the after-image digest checked before any publication.
	SHA256 string `json:"sha256"`
}

// NewPut snapshots the exact replacement bytes and hashes the prior bytes.
// A nil previous slice means absent; a non-nil empty slice means an empty file.
func NewPut(path string, previous, next []byte) Put {
	p := Put{Path: path, Data: bytes.Clone(next), SHA256: batchDigest(next)}
	if previous != nil {
		p.PreviousSHA256 = batchDigest(previous)
	}
	return p
}

// Batch is a schema-1 redo intent with 1..32 ordered puts. Every after-image
// is at most 128 KiB; its complete base64-encoded intent is at most 1 MiB.
// It adds no transaction semantics to ordinary journal writes by itself.
type Batch struct {
	// Schema is the generic intent format version; currently one.
	Schema int `json:"schema"`
	// ID is a bounded lowercase identifier chosen once by the caller.
	ID string `json:"id"`
	// Puts are applied in order only after all targets pass validation.
	Puts []Put `json:"puts"`
}

// BatchLog publishes and replays one intent below Root. Root and every target
// parent must already exist without symlink components. Intent is relative.
// The caller holds its transaction lock throughout each call, creates/syncs
// directories, fences formats, and validates application-specific semantics.
// No caller may start a later mutation while this intent remains unresolved.
type BatchLog struct {
	// Root is the existing caller-owned state directory.
	Root string
	// Intent is the relative path of this caller's single pending intent.
	Intent string
	io     *batchIO
}

type batchIO struct {
	write  func(string, []byte) error
	remove func(string) error
	sync   func(string) error
}

func (l BatchLog) operations() batchIO {
	if l.io != nil {
		return *l.io
	}
	return batchIO{WriteAtomic, os.Remove, SyncDir}
}

// Commit durably publishes an immutable intent before applying its puts.
// Retrying the identical pending batch is allowed; replacing it is refused.
// Any error may leave a published intent, including a rename/sync ambiguity;
// callers must recover or stop, never assume that an error means no commit.
func (l BatchLog) Commit(batch Batch) error {
	encoded, err := batch.encode()
	if err != nil {
		return err
	}
	if err := l.validatePaths(batch); err != nil {
		return err
	}
	pending, err := l.read()
	if err == nil {
		if !bytes.Equal(bytes.TrimSpace(pending), bytes.TrimSpace(encoded)) {
			return ErrBatchConflict
		}
		return l.apply(batch)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := l.preflight(batch); err != nil {
		return err
	}
	if err := l.operations().write(l.intentPath(), encoded); err != nil {
		return err
	}
	return l.apply(batch)
}

// Recover validates the entire intent and all prior-or-new targets before
// replaying anything. The optional check is the caller's pure domain check;
// it must not mutate batch bytes, perform callbacks or publish state.
// A missing intent is a successful no-op. Recovery preserves event IDs/bytes.
func (l BatchLog) Recover(check func(Batch) error) error {
	raw, err := l.read()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var batch Batch
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&batch); err != nil {
		return fmt.Errorf("%w: malformed intent", ErrInvalidBatch)
	}
	if _, err := batch.encode(); err != nil {
		return err
	}
	if err := l.validatePaths(batch); err != nil {
		return err
	}
	if check != nil {
		if err := check(batch); err != nil {
			return err
		}
	}
	return l.apply(batch)
}

func (b Batch) encode() ([]byte, error) {
	if b.Schema != 1 || !validBatchID(b.ID) || len(b.Puts) == 0 || len(b.Puts) > maxBatchPuts {
		return nil, ErrInvalidBatch
	}
	seen := make(map[string]bool, len(b.Puts))
	for _, put := range b.Puts {
		if seen[put.Path] || !safeBatchPath(put.Path) || len(put.Data) > maxPutBytes {
			return nil, ErrInvalidBatch
		}
		if put.SHA256 != batchDigest(put.Data) || (put.PreviousSHA256 != "" && !validDigest(put.PreviousSHA256)) {
			return nil, ErrInvalidBatch
		}
		seen[put.Path] = true
	}
	return EncodeJSON(b, maxBatchBytes)
}

func validBatchID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, ch := range id {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
			return false
		}
	}
	return true
}

func safeBatchPath(path string) bool {
	return len(path) <= 4096 && fsutil.SafeRelativePath(path) && !strings.ContainsAny(path, "\\\x00")
}

func batchDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validDigest(digest string) bool {
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(digest) == digest
}

func (l BatchLog) intentPath() string { return filepath.Join(l.Root, l.Intent) }

func (l BatchLog) read() ([]byte, error) {
	if !safeBatchPath(l.Intent) {
		return nil, ErrInvalidBatch
	}
	if err := l.checkParents(l.Intent); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := ReadJSON(l.intentPath(), maxBatchBytes, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (l BatchLog) validatePaths(batch Batch) error {
	if !safeBatchPath(l.Intent) {
		return ErrInvalidBatch
	}
	if err := l.checkParents(l.Intent); err != nil {
		return err
	}
	for _, put := range batch.Puts {
		if put.Path == l.Intent || strings.HasPrefix(put.Path, l.Intent+"/") || strings.HasPrefix(l.Intent, put.Path+"/") {
			return ErrInvalidBatch
		}
		if err := l.checkParents(put.Path); err != nil {
			return err
		}
	}
	return nil
}

func (l BatchLog) checkParents(path string) error {
	current := l.Root
	parts := append([]string{""}, strings.Split(filepath.Dir(path), string(filepath.Separator))...)
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrInvalidBatch
		}
	}
	return nil
}

func (l BatchLog) preflight(batch Batch) ([]bool, error) {
	already := make([]bool, len(batch.Puts))
	for i, put := range batch.Puts {
		var raw []byte
		path := filepath.Join(l.Root, put.Path)
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() > maxPutBytes {
				return nil, ErrInvalidBatch
			}
			raw, err = os.ReadFile(path) //nolint:gosec // G304: paths were validated below the caller-owned root.
		}
		if errors.Is(err, os.ErrNotExist) && put.PreviousSHA256 == "" {
			continue
		}
		if err != nil {
			return nil, err
		}
		digest := batchDigest(raw)
		already[i] = digest == put.SHA256
		if !already[i] && digest != put.PreviousSHA256 {
			return nil, ErrBatchConflict
		}
	}
	return already, nil
}

func (l BatchLog) apply(batch Batch) error {
	ops := l.operations()
	// A prior rename may have succeeded while its directory sync failed.
	// Confirm intent durability before publishing any after-image on replay.
	if err := ops.sync(filepath.Dir(l.intentPath())); err != nil {
		return err
	}
	already, err := l.preflight(batch)
	if err != nil {
		return err
	}
	for i, put := range batch.Puts {
		path := filepath.Join(l.Root, put.Path)
		if already[i] {
			err = ops.sync(filepath.Dir(path))
		} else {
			err = ops.write(path, put.Data)
		}
		if err != nil {
			return err
		}
	}
	if err := ops.remove(l.intentPath()); err != nil {
		return err
	}
	return ops.sync(filepath.Dir(l.intentPath()))
}
