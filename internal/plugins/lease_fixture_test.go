package plugins

import (
	"errors"
	"os"
	"syscall"
)

// Keep these subprocess-fixture checks identical to pluginfeed's private lease
// validator. Raw FD3 stays open through process exit; validation never releases it.
func checkFixtureLeaseFD3() error {
	var info syscall.Stat_t
	if err := syscall.Fstat(3, &info); err != nil {
		return err
	}
	if info.Mode&syscall.S_IFMT != syscall.S_IFREG || info.Size != 0 || info.Mode&0077 != 0 || info.Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t.
		return errors.New("plugin lease must be an empty private regular file owned by this user")
	}
	return nil
}
