package bus

import (
	"errors"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validNativeConfigRoot(root string) bool {
	return filepath.IsAbs(root) && utf8.ValidString(root) && !strings.ContainsFunc(root, unicode.IsControl)
}

func registrationConfigRoot(adapter agent.Adapter) (string, error) {
	names, ok := adapter.(agent.NameAdapter)
	if !ok {
		return "", nil
	}
	root := names.ConfigDir()
	if !validNativeConfigRoot(root) {
		return "", model.NewError(model.CodeInvalidInput, "%s", nativeProfileHint(adapter))
	}
	return filepath.Clean(root), nil
}

func validateStoredConfigRoot(s Store, v model.Session) error {
	adapter, err := s.registry.Lookup(v.Kind)
	if err != nil {
		return err
	}
	if _, ok := adapter.(agent.NameAdapter); ok && !validNativeConfigRoot(v.NativeConfigDir) {
		return errSessionMissingRoot
	}
	return nil
}

func checkCompactConfigRoot(current, next model.Session, compact bool) error {
	if compact && !current.Retired && next.NativeConfigDir != "" && current.NativeConfigDir != next.NativeConfigDir {
		return model.NewError(model.CodeConflict, "native profile changed during compaction; start a fresh session to register this profile")
	}
	return nil
}

// Preserve the existing identity/read-error order before considering a profile.
func registrationReadError(current model.Session, native string, err error) error {
	if err == nil && current.NativeID != native {
		return model.NewError(model.CodeConflict, "native session identity collision")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
