package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
)

type checkObservation struct {
	PID              int
	Home, Cwd        string
	EnvironmentCount int
}

func checkTestChild(mode, report string) error {
	if err := journal.CheckPluginLeaseFD(3); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(checkObservation{os.Getpid(), os.Getenv("HOME"), cwd, len(os.Environ())})
	if err != nil {
		return err
	}
	if err := journal.WriteAtomic(report, raw); err != nil {
		return err
	}
	if mode == "exit" {
		return nil
	}
	if mode == "ignore-eof" {
		mode = "hold-eof"
	}
	if mode == "close-stdout" {
		mode = "close-stdout-hold-eof"
	}
	if err := os.WriteFile(filepath.Join(cwd, ".child-mode"), []byte(mode), 0600); err != nil {
		return err
	}
	return fakeChild(filepath.Join(cwd, "outbox"))
}

func checkFixture(t *testing.T, mode string) (CheckTarget, string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(t.TempDir(), "observed.json")
	launch, err := bus.NewPluginLaunch(binary, []string{"check-test-child", mode, report})
	if err != nil {
		t.Fatal(err)
	}
	return CheckTarget{Launch: &launch}, report
}

func TestPluginCheckObservesTransportWithPrivateHomeCwdAndReap(t *testing.T) {
	target, path := checkFixture(t, "healthy")
	var output bytes.Buffer
	if err := Check(t.Context(), target, &output); err != nil {
		t.Fatal(output.String(), err)
	}
	expected := "PASS launch\nPASS hello\nPASS enabled-gap\nPASS event-1\nPASS redelivery\nPASS event-2\nPASS stdin-eof\nPASS reaped\n"
	if output.String() != expected {
		t.Fatal("stage report misstates observations", output.String())
	}
	var observed checkObservation
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &observed) != nil {
		t.Fatal("missing observation", err)
	}
	if observed.EnvironmentCount != 1 || observed.Home == os.Getenv("HOME") || filepath.Dir(observed.Home) != filepath.Dir(observed.Cwd) {
		t.Fatal("check inherited real environment or cwd", observed)
	}
	if root := filepath.Dir(observed.Cwd); strings.HasPrefix(filepath.Base(root), "zalki-plugin-check-") {
		defer os.RemoveAll(root)
	}
	if _, err := os.Stat(observed.Cwd); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary cwd remains after reap", err)
	}
	if err := syscall.Kill(observed.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("check child remains alive", err)
	}
}

func TestPluginCheckReportsObservedFailuresAndAlwaysReaps(t *testing.T) {
	cases := map[string]string{"exit": "FAIL hello", "no-hello": "FAIL hello", "bad-hello": "FAIL hello", "bad-ack": "FAIL enabled-gap", "ignore-eof": "FAIL stdin-eof", "close-stdout": "FAIL stream", "oversize": "FAIL stream"}
	for mode, stage := range cases {
		t.Run(mode, func(t *testing.T) {
			target, path := checkFixture(t, mode)
			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			started := time.Now()
			err := Check(ctx, target, &output)
			if err == nil || !strings.Contains(output.String(), stage) || !strings.Contains(output.String(), "PASS reaped\n") {
				t.Fatal("failure/reap observation missing", output.String(), err)
			}
			if time.Since(started) > 12*time.Second {
				t.Fatal("check failed to complete within generous lifecycle bound")
			}
			var observed checkObservation
			raw, readErr := os.ReadFile(path)
			if readErr != nil || json.Unmarshal(raw, &observed) != nil {
				t.Fatal(readErr)
			}
			if err := syscall.Kill(observed.PID, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatal("failed child not reaped", err)
			}
		})
	}
}

func TestPluginCheckFailureOutputStillReaps(t *testing.T) {
	target, path := checkFixture(t, "healthy")
	if err := Check(t.Context(), target, &failingCheckOutput{}); err == nil {
		t.Fatal("output failure ignored")
	}
	var observed checkObservation
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &observed) != nil {
		t.Fatal("child did not reach hello before output cut", err)
	}
	if err := syscall.Kill(observed.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("output failure left child alive", err)
	}
}

type failingCheckOutput struct{ calls int }

func (w *failingCheckOutput) Write(data []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return len(data), nil
	}
	return 0, io.ErrClosedPipe
}

type framingVector struct {
	ID     string   `json:"id"`
	Input  string   `json:"input_utf8"`
	Chunks []string `json:"chunks_utf8"`
	Expect string   `json:"expect"`
	Frames []frame  `json:"expect_frames"`
}

func TestConformanceFramingVectorsPreserveCurrentDecodedSemantics(t *testing.T) {
	file, err := os.Open("testdata/conformance/framing.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	for {
		var row framingVector
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Run(row.ID, func(t *testing.T) { runFramingVector(t, row) })
	}
}

func runFramingVector(t *testing.T, row framingVector) {
	t.Helper()
	parts := make([]io.Reader, 0, len(row.Chunks)+1)
	if len(row.Chunks) == 0 {
		parts = append(parts, strings.NewReader(row.Input))
	}
	if len(row.Chunks) > 0 {
		for _, part := range row.Chunks {
			parts = append(parts, strings.NewReader(part))
		}
	}
	reader := bufio.NewReaderSize(io.MultiReader(parts...), maxFrame+1)
	if len(row.Frames) > 0 {
		for _, expected := range row.Frames {
			got, err := readFrame(reader)
			if err != nil || !reflect.DeepEqual(got, expected) {
				t.Fatal("multi-frame boundary changed", err)
			}
		}
		return
	}
	value, err := readFrame(reader)

	accepted := map[string]bool{
		"decode":                              err == nil,
		"decode_one_frame":                    err == nil,
		"last_key_wins_in_current_go_decoder": err == nil && value.validHello(),
		"decode_then_semantic_reject":         err == nil && !value.validHello(),
		"eof":                                 errors.Is(err, io.EOF),
		"unexpected_eof":                      errors.Is(err, io.ErrUnexpectedEOF),
		"reject_not_clean_eof":                err != nil && !errors.Is(err, io.EOF),
		"reject":                              err != nil,
	}
	ok, known := accepted[row.Expect]
	if !known {
		t.Fatal("unhandled vector expectation", row.Expect)
	}
	if !ok {
		t.Fatal("decoder expectation changed", row.Expect, err)
	}

}

func TestConformanceReferenceVectorsUseOnlyReferenceOutboxOracle(t *testing.T) {
	file, err := os.Open("testdata/conformance/reference-notifier.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	for {
		var row struct {
			ID    string `json:"id"`
			Input string `json:"host_stdin_utf8"`
		}
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if row.ID == "state_change" || row.ID == "duplicate_same_event" || row.ID == "duplicate_id_different_event_bytes" || row.ID == "registration_ack_no_notification" {
			t.Run(row.ID, func(t *testing.T) { runReferenceVector(t, row.ID, row.Input) })
		}
	}
}

func runReferenceVector(t *testing.T, id, input string) {
	t.Helper()
	outbox := filepath.Join(notifierTemp(t), "outbox")
	if err := prepareOutbox(outbox); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReaderSize(strings.NewReader(input), maxFrame+1)
	calls := 0
	var last error
	for {
		value, err := readFrame(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		last = notify(outbox, value)
		calls++
		if last != nil {
			break
		}
	}
	if id == "duplicate_id_different_event_bytes" {
		if last == nil || calls != 2 {
			t.Fatal("conflicting replay accepted", last, calls)
		}
	} else if last != nil {
		t.Fatal(last)
	}
	wantCalls := 1
	if strings.HasPrefix(id, "duplicate_") {
		wantCalls = 2
	}
	if calls != wantCalls {
		t.Fatal("vector delivery count changed", calls, wantCalls)
	}
	files, err := os.ReadDir(outbox)
	if err != nil {
		t.Fatal(err)
	}
	want := 1
	if id == "registration_ack_no_notification" {
		want = 0
	}
	if len(files) != want {
		t.Fatal("reference-only outbox oracle mismatch", len(files), want)
	}
}
