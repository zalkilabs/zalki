package pluginfeed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// PluginLaunch records drift checks for one executable and its exact literal argv.
// PinSHA256 is not authenticity: the same user can rewrite this record and its checks.
// Interpreters, parent-directory symlinks and replacement before exec remain external.
type PluginLaunch struct {
	Executable string   `json:"exec"`
	Args       []string `json:"args"`
	SHA256     string   `json:"sha256"`
	PinSHA256  string   `json:"pin_sha256"`
}

// NewPluginLaunch returns a validated drift-checked launch without installing it.
func NewPluginLaunch(executable string, args []string) (PluginLaunch, error) {
	launch := PluginLaunch{Executable: executable, Args: args}
	err := launch.pin()
	return launch, err
}

func (l PluginLaunch) validate() error {
	if !filepath.IsAbs(l.Executable) || filepath.Clean(l.Executable) != l.Executable || !utf8.ValidString(l.Executable) || strings.ContainsRune(l.Executable, 0) || len(l.Args) > 8 {
		return model.NewError(model.CodeInvalidInput, "plugin requires a clean absolute executable and at most 8 arguments")
	}
	size := 0
	for _, arg := range l.Args {
		size += len(arg)
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return model.NewError(model.CodeInvalidInput, "plugin arguments must be UTF-8 without NUL")
		}
	}
	if size > 1024 {
		return model.NewError(model.CodeInvalidInput, "plugin arguments exceed 1024 bytes")
	}
	return nil
}

func validPluginSHA(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == sha256.Size && value == hex.EncodeToString(data)
}

func (l PluginLaunch) pinDigest() (string, error) {
	l.PinSHA256 = ""
	raw, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func pluginExecutableSHA(path string) (string, error) { return pluginExecutableSHAObserved(path, nil) }

func pluginExecutableSHAObserved(path string, observe func(*os.File)) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), path)
	if observe != nil {
		observe(file)
	}
	defer file.Close() //nolint:errcheck // Read-only executable hashing.
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", model.NewError(model.CodeInvalidInput, "plugin executable must be a regular executable file, not a symlink")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (l *PluginLaunch) pin() error {
	l.Args = slices.Clone(l.Args)
	if l.Args == nil {
		l.Args = []string{}
	}
	if err := l.validate(); err != nil {
		return err
	}
	var err error
	l.SHA256, err = pluginExecutableSHA(l.Executable)
	if err != nil {
		return err
	}
	l.PinSHA256, err = l.pinDigest()
	return err
}

// Verify refuses executable or argument drift without silently changing the pin.
func (l PluginLaunch) Verify() error {
	if err := l.validate(); err != nil {
		return err
	}
	pin, err := l.pinDigest()
	if err != nil {
		return err
	}
	actual, err := pluginExecutableSHA(l.Executable)
	if err != nil {
		return err
	}
	if pin != l.PinSHA256 || actual != l.SHA256 {
		return model.NewError(model.CodeConflict, "plugin launch changed; disable and enable to pin it again")
	}
	return nil
}

func (s PluginFeed) preparePluginPin() (*PluginLaunch, [32]byte, error) {
	var launch *PluginLaunch
	var prior [32]byte
	err := bus.WithJournalRead(s.store, func(bus.JournalView) error {
		p, raw, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if !p.Enabled && p.Launch != nil {
			launch = p.Launch
			prior = sha256.Sum256(raw)
		}
		return nil
	})
	if err == nil && launch != nil {
		err = launch.pin()
	}
	return launch, prior, err
}
