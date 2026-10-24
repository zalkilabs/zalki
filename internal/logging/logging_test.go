package logging

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLevelsAndProcessLocalConfiguration(t *testing.T) {
	global := slog.Default()
	for _, level := range []string{"debug", "info", "warn", "error", "off"} {
		t.Run(level, func(t *testing.T) {
			var output bytes.Buffer
			logger, err := New(Config{Level: level, File: "-"}, &output)
			if err != nil {
				t.Fatal(err)
			}
			logger.Debug("debug")
			logger.Info("info")
			logger.Warn("warn")
			logger.Error("error")
			rows := decodeLines(t, output.Bytes())
			want := map[string]int{"debug": 4, "info": 3, "warn": 2, "error": 1, "off": 0}[level]
			if len(rows) != want {
				t.Fatalf("%s emitted %d entries, want %d", level, len(rows), want)
			}
			for _, row := range rows {
				if row["time"] == nil || row["level"] == nil || row["msg"] == nil {
					t.Fatal("missing standard slog fields", row)
				}
			}
		})
	}
	if slog.Default() != global {
		t.Fatal("configuration replaced the global logger")
	}
	if _, err := New(Config{Level: "invalid", File: "-"}, nil); err == nil {
		t.Fatal("invalid level accepted")
	}
	if _, err := New(Config{Level: "info", File: "relative/path"}, nil); err == nil {
		t.Fatal("relative log path accepted")
	}
}

func decodeLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var rows []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatalf("invalid JSON log entry: %v", err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestPrivateFileRotationAndLazyCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "zalki.jsonl")
	logger, err := New(Config{Level: "error", File: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("filtered")
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("disabled event created log files", err)
	}
	writer := &rotatingWriter{path: path, maxBytes: 128, backups: 3}
	line := []byte(`{"msg":"event","padding":"` + strings.Repeat("x", 60) + `"}` + "\n")
	for range 20 {
		if _, err := writer.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{path, path + ".1", path + ".2", path + ".3"} {
		info, err := os.Stat(name)
		if err != nil || info.Size() > 128 || info.Mode().Perm() != 0600 {
			t.Fatal("invalid private rotated log", info, err)
		}
		data, _ := os.ReadFile(name)
		if len(decodeLines(t, data)) != 1 {
			t.Fatal("rotation split a JSON entry")
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatal("retention was not bounded", err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("log directory is not private", err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatal("rotation removed the lock inode", err)
	}
	if _, err := writer.Write(make([]byte, 129)); err == nil {
		t.Fatal("oversized entry bypassed the limit")
	}
}

func TestFileFailureFallsBackWithoutLeakingErrorOrFailingCaller(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "zalki.jsonl")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	var fallback bytes.Buffer
	logger, err := New(Config{Level: "info", File: path}, &fallback)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("first")
	logger.Info("second")
	rows := decodeLines(t, fallback.Bytes())
	if len(rows) != 3 || rows[0]["msg"] != "logging.fallback" || rows[1]["msg"] != "first" || rows[2]["msg"] != "second" {
		t.Fatal("fallback lost or duplicated events", rows)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "keep" {
		t.Fatal("followed log symlink")
	}
	if strings.Contains(fallback.String(), target) || strings.Contains(fallback.String(), path) {
		t.Fatal("fallback logged private paths")
	}
	var direct bytes.Buffer
	w := fallbackWriter{primary: failedWriter{}, fallback: &direct}
	if n, err := w.Write([]byte("safe event\n")); n != 11 || err != nil || strings.Contains(direct.String(), "private-error-sentinel") {
		t.Fatal("sink failure leaked or propagated", n, err)
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("private-error-sentinel") }

func TestDefaultSinkDoesNotCreateMissingState(t *testing.T) {
	state := filepath.Join(t.TempDir(), "missing")
	logger, err := New(Config{Level: "debug", File: filepath.Join(state, "logs", "zalki.jsonl"), ExistingStateDir: state}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger.Error("early argument failure")
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("logging created a journal for an invalid command", err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	logger.Info("journal now exists")
	if _, err := os.Stat(filepath.Join(state, "logs", "zalki.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestLogWriterProcessHelper(t *testing.T) {
	path := os.Getenv("ZALKI_LOG_TEST_PATH")
	if path == "" {
		return
	}
	w := rotatingWriter{path: path, maxBytes: 2048, backups: 3}
	for i := range 20 {
		line := fmt.Sprintf("{\"worker\":%q,\"seq\":%d,\"padding\":%q}\n", os.Getenv("ZALKI_LOG_TEST_WORKER"), i, strings.Repeat("x", 48))
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRotationSerializesAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "zalki.jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLogWriterProcessHelper$")
			cmd.Env = append(os.Environ(), "ZALKI_LOG_TEST_PATH="+path, fmt.Sprintf("ZALKI_LOG_TEST_WORKER=%d", i))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("log writer process failed: %v %s", err, out)
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, name := range []string{path, path + ".1", path + ".2", path + ".3"} {
		data, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > 2048 {
			t.Fatal("concurrent writers bypassed the size limit")
		}
		for _, row := range decodeLines(t, data) {
			key := fmt.Sprint(row["worker"], "/", row["seq"])
			if seen[key] {
				t.Fatal("duplicate entry", key)
			}
			seen[key] = true
		}
	}
	if len(seen) != 60 {
		t.Fatalf("concurrent rotation lost entries within retention: %d", len(seen))
	}
}
