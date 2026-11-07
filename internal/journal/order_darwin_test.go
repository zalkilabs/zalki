package journal

import (
	"errors"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBarrierSyncFallbackIsFullOrError(t *testing.T) {
	for _, result := range []error{nil, unix.ENOTSUP} {
		var commands []int
		err := barrierSync(1, func(_ uintptr, command, _ int) (int, error) {
			commands = append(commands, command)
			if command == unix.F_BARRIERFSYNC {
				return -1, unix.ENOTSUP
			}
			return 0, result
		})
		if !errors.Is(err, result) || !reflect.DeepEqual(commands, []int{unix.F_BARRIERFSYNC, unix.F_FULLFSYNC}) {
			t.Fatalf("fallback commands=%v, error=%v", commands, err)
		}
	}
}

func TestBarrierSyncRetriesInterrupts(t *testing.T) {
	count := 0
	err := barrierSync(1, func(_ uintptr, command, _ int) (int, error) {
		count++
		if command != unix.F_BARRIERFSYNC {
			t.Fatal("interrupt selected an unexpected fallback")
		}
		if count == 1 {
			return -1, unix.EINTR
		}
		return 0, nil
	})
	if err != nil || count != 2 {
		t.Fatalf("retry count=%d, error=%v", count, err)
	}
}
