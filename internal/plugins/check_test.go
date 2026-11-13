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

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

type checkObservation struct {
	PID              int
	Home, Cwd        string
	EnvironmentCount int
}

func checkTestChild(mode, report string) error {
	if err := pluginfeed.CheckPluginLeaseFD(3); err != nil {
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
	return fakeChild(cwd)
}

func checkFixture(t *testing.T, mode string) (CheckTarget, string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(t.TempDir(), "observed.json")
	launch, err := pluginfeed.NewPluginLaunch(binary, []string{"check-test-child", mode, report})
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
	if observed.EnvironmentCount != 2 || observed.Home == os.Getenv("HOME") || filepath.Dir(observed.Home) != filepath.Dir(observed.Cwd) {
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

// check uses the same hello deadline as run: a slow first frame still passes.
func TestPluginCheckAcceptsASlowHello(t *testing.T) {
	target, _ := checkFixture(t, "slow-hello")
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), helloTimeout+15*time.Second)
	defer cancel()
	if err := Check(ctx, target, &output); err != nil || !strings.Contains(output.String(), "PASS hello\n") {
		t.Fatal("slow hello failed check", output.String(), err)
	}
}

func TestPluginCheckReportsObservedFailuresAndAlwaysReaps(t *testing.T) {
	cases := map[string]string{"exit": "FAIL hello", "no-hello": "FAIL hello", "bad-hello": "FAIL hello", "bad-ack": "FAIL enabled-gap", "ignore-eof": "FAIL stdin-eof", "close-stdout": "FAIL stream", "oversize": "FAIL stream", "v1-hello": "FAIL hello", "needs-members": "FAIL hello"}
	for mode, stage := range cases {
		t.Run(mode, func(t *testing.T) {
			target, path := checkFixture(t, mode)
			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), helloTimeout+15*time.Second)
			defer cancel()
			started := time.Now()
			err := Check(ctx, target, &output)
			if err == nil || !strings.Contains(output.String(), stage) || !strings.Contains(output.String(), "PASS reaped\n") {
				t.Fatal("failure/reap observation missing", output.String(), err)
			}
			// no-hello waits for the hello deadline, then the EOF grace and kill.
			if time.Since(started) > helloTimeout+8*time.Second {
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
	file, err := os.Open("testdata/plugin-wire/framing.jsonl")
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
		"last_key_wins_in_current_go_decoder": err == nil && isHello(value),
		"decode_then_semantic_reject":         err == nil && !isHello(value),
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
