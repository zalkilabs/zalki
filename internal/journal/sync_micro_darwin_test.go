package journal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSyncMicroLatency(t *testing.T) {
	if os.Getenv("ZALKI_SYNC_MICRO") != "1" {
		t.Skip("opt-in quiet-window native sync benchmark")
	}
	dir := t.TempDir()
	file, err := os.OpenFile(filepath.Join(dir, "entry-a"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	for _, kind := range []string{"file", "directory"} {
		for _, dirty := range []bool{false, true} {
			for _, mode := range []string{"full", "barrier"} {
				measureNativeSync(t, dir, file, directory, kind, dirty, mode)
			}
		}
	}
}

func nativeBenchmarkSync(file *os.File, mode string) (bool, error) {
	if mode == "full" {
		return false, file.Sync()
	}
	var err error
	for {
		_, err = unix.FcntlInt(file.Fd(), unix.F_BARRIERFSYNC, 0)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	fallback := errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENOTSUP)
	if fallback {
		_, err = unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0)
	}
	runtime.KeepAlive(file)
	return fallback, err
}

func measureNativeSync(t *testing.T, dir string, file, directory *os.File, kind string, dirty bool, mode string) {
	t.Helper()
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := directory.Sync(); err != nil {
		t.Fatal(err)
	}
	samples := make([]int64, 0, 128)
	fallbacks := 0
	current := filepath.Join(dir, "entry-a")
	for i := range 128 {
		target := file
		if kind == "directory" {
			target = directory
		}
		if dirty {
			current = dirtySyncInput(t, dir, current, file, kind, i)
		}
		start := time.Now()
		fallback, err := nativeBenchmarkSync(target, mode)
		elapsed := time.Since(start).Nanoseconds()
		if err != nil {
			t.Fatal(err)
		}
		if fallback {
			fallbacks++
		}
		samples = append(samples, elapsed)
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	raw, err := json.Marshal(map[string]any{"kind": kind, "dirty": dirty, "mode": mode, "samples": len(samples),
		"median_ns": float64(sorted[63]+sorted[64]) / 2, "p95_ns": sorted[121], "app_fallbacks": fallbacks, "raw_ns": samples})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SYNC_MICRO_JSON %s", raw)
}

func dirtySyncInput(t *testing.T, dir, current string, file *os.File, kind string, sample int) string {
	t.Helper()
	if kind == "file" {
		if _, err := file.WriteAt([]byte{byte(sample)}, 0); err != nil {
			t.Fatal(err)
		}
		return current
	}
	next := filepath.Join(dir, "entry-b")
	if filepath.Base(current) == "entry-b" {
		next = filepath.Join(dir, "entry-a")
	}
	if err := os.Rename(current, next); err != nil {
		t.Fatal(err)
	}
	return next
}
