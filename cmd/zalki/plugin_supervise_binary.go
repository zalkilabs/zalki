package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// exitReplaced (75) tells launchd that a new controller should take over:
// the Zalki executable changed under the running one.
const exitReplaced = 75

// binaryMissingGrace is how long a missing executable (a Brew replacement
// in progress) is retried before the controller gives up with exit 0.
const binaryMissingGrace = 30 * time.Second

var errZalkiReplaced = errors.New("the zalki executable changed; run zalki plugins supervise again")

// stableBinary is the launcher path B: an absolute argv[0] (launchd passes
// the stable Brew link), else the running executable.
func stableBinary() (string, error) {
	if len(os.Args) > 0 && filepath.IsAbs(os.Args[0]) && filepath.Clean(os.Args[0]) == os.Args[0] {
		return os.Args[0], nil
	}
	binary, err := os.Executable()
	if err != nil || !filepath.IsAbs(binary) {
		return "", errors.New("zalki plugins supervise cannot find its own executable")
	}
	return binary, nil
}

// binaryIdentity is the file B resolves to now.
type binaryIdentity struct {
	dev, ino uint64
	size     int64
	modified int64
}

func resolveBinary(path string) (binaryIdentity, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return binaryIdentity{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return binaryIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return binaryIdentity{}, errors.New("no file identity")
	}
	return binaryIdentity{statDevice(stat), stat.Ino, stat.Size, info.ModTime().UnixNano()}, nil
}

// binaryCheck is the outcome of one identity check.
type binaryCheck int

const (
	binarySame binaryCheck = iota
	binaryChanged
	binaryMissing
	binaryGone    // missing for longer than the grace
	binaryUnknown // cannot be checked (not "missing"): exit 1, a new supervisor retries
)

// checkBinary compares B with the identity it had at start. A missing B is
// retried for the grace; it is gone only after that.
func (s *supervisor) checkBinary() binaryCheck {
	now, err := s.resolve(s.binary)
	switch {
	case err == nil && now == s.binaryID:
		s.missingSince = nil
		return binarySame
	case err == nil:
		return binaryChanged
	case !errors.Is(err, os.ErrNotExist):
		return binaryUnknown
	}
	if s.missingSince == nil {
		at := s.now()
		s.missingSince = &at
	}
	if s.now().Sub(*s.missingSince) >= binaryMissingGrace {
		return binaryGone
	}
	return binaryMissing
}
