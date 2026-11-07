package journal

import (
	"errors"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// orderBatchFile submits data and orders it before later namespace writes.
// It is not the durable commit point; the intent keeps its full syncs.
func orderBatchFile(file *os.File) error {
	err := barrierSync(file.Fd(), unix.FcntlInt)
	runtime.KeepAlive(file)
	return err
}

func barrierSync(fd uintptr, fcntl func(uintptr, int, int) (int, error)) error {
	err := callSyncCommand(fd, unix.F_BARRIERFSYNC, fcntl)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOTTY) {
		err = callSyncCommand(fd, unix.F_FULLFSYNC, fcntl)
	}
	return err
}

func callSyncCommand(fd uintptr, command int, fcntl func(uintptr, int, int) (int, error)) error {
	for {
		_, err := fcntl(fd, command, 0)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
