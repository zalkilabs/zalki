package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"golang.org/x/sys/unix"
)

const readTestBody = "B2-PRIVATE-RESPONSE-BODY <>& secret"

type readTestOutcome struct {
	Request string `json:"request_id"`
	Code    string `json:"code"`
	Message *struct {
		ID   string  `json:"id"`
		From string  `json:"from"`
		To   string  `json:"to"`
		Kind string  `json:"kind,omitempty"`
		Body *string `json:"body,omitempty"`
	} `json:"message,omitempty"`
}

type readFixture struct {
	store          bus.Store
	feed           pluginfeed.PluginFeed
	session        model.Session
	message        model.Message
	binary, result string
	output, logs   bytes.Buffer
}

func readHostFixture(t *testing.T, mode string, body string, nativePending bool) *readFixture {
	t.Helper()
	store, session := hostStore(t)
	x := &readFixture{store: store, session: session, result: filepath.Join(t.TempDir(), "result.json")}
	x.store.Logger = slog.New(slog.NewTextHandler(&x.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	x.binary = binary
	x.feed = pluginfeed.NewPluginFeed(x.store, "reader")
	if err := x.feed.AddExternalPlugin(binary, []string{"read-test-child", mode, x.result, session.ID, "10000000-0000-4000-8000-000000000001"}); err != nil {
		t.Fatal(err)
	}
	if err := x.feed.ConfigurePluginFeed([]string{session.ID}); err != nil {
		t.Fatal(err)
	}
	if err := x.feed.GrantPluginCapabilities(pluginfeed.PluginCapabilityGrants{Read: []string{session.ID}}); err != nil {
		t.Fatal(err)
	}
	x.message = model.Message{Schema: 1, ID: "10000000-0000-4000-8000-000000000001", From: session.ID, To: session.ID, TargetGeneration: session.Generation, Body: body, CreatedAt: "2000-01-01T00:00:00Z", AcknowledgedAt: "2000-01-02T00:00:00Z", Status: model.StatusSubmitted}
	if nativePending {
		x.message.AcknowledgedAt = ""
	}
	raw, err := journal.EncodeJSON(x.message, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(store.Dir, "messages", x.message.ID+".json"), raw); err != nil {
		t.Fatal(err)
	}
	if err := x.feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	return x
}

func (x *readFixture) start(t *testing.T) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, x.store, x.binary, "reader", &x.output); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("read host not reaped")
		}
	})
	return cancel, done
}

func awaitReadResult(t *testing.T, path string) readTestOutcome {
	t.Helper()
	var result readTestOutcome
	eventually(t, func() bool { raw, err := os.ReadFile(path); return err == nil && json.Unmarshal(raw, &result) == nil })
	return result
}

func awaitReadStop(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(4 * time.Second):
		t.Fatal("read transport did not stop")
		return nil
	}
}

func writeReadCommand(out io.Writer, session, message string) error {
	raw, err := json.Marshal(readCommand{Read: &readRequest{Session: session, Message: message}})
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = out.Write(raw)
	return err
}

func readTestChild(args []string) error {
	if err := pluginfeed.CheckPluginLeaseFD(3); err != nil {
		return err
	}
	mode, result, session, message := args[0], args[1], args[2], args[3]
	in, err := readChildHandshake(mode, result)
	if err != nil {
		return err
	}
	if mode == "monitor" {
		if err := journal.WriteAtomic(result+".acked", []byte("yes")); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, in)
		return err
	}
	if strings.HasPrefix(mode, "blocked-") {
		return blockedReadChild(mode, result, session, message)
	}
	if err := sendReadFixture(mode, session, message); err != nil {
		return err
	}
	return finishReadFixture(mode, result, session, message, in)
}

func sendReadFixture(mode, session, message string) error {
	switch mode {
	case "pipeline":
		var b bytes.Buffer
		if err := writeReadCommand(&b, session, message); err != nil {
			return err
		}
		if err := writeReadCommand(&b, session, message); err != nil {
			return err
		}
		if _, err := os.Stdout.Write(b.Bytes()); err != nil {
			return err
		}
	case "oversize-request", "request-limit":
		limit := 4096
		if mode == "oversize-request" {
			limit++
		}
		return sizedReadFixture(session, message, limit)

	case "injected-binding":
		if _, err := fmt.Fprintf(os.Stdout, "{\"read\":{\"session\":%q,\"message\":%q},\"binding\":\"FORBIDDEN-CHILD-CLAIM\"}\n", session, message); err != nil {
			return err
		}
	default:
		if err := writeReadCommand(os.Stdout, session, message); err != nil {
			return err
		}
	}
	return nil
}

func finishReadFixture(mode, result, session, message string, in *bufio.Reader) error {

	raw, err := in.ReadBytes('\n')
	if err != nil {
		raw = []byte(`{"code":"eof"}`)
	}
	if err := journal.WriteAtomic(result, raw); err != nil {
		return err
	}
	var received readTestOutcome
	if json.Unmarshal(raw, &received) != nil {
		return errors.New("invalid result")
	}
	if mode == "echo-stderr" && received.Message != nil && received.Message.Body != nil {
		fmt.Fprintln(os.Stderr, *received.Message.Body)
		return errors.New("fixture exits after body echo")
	}
	if mode == "rate" && received.Code == "ok" {
		if err := writeReadCommand(os.Stdout, session, message); err != nil {
			return err
		}
		next, err := in.ReadBytes('\n')
		if err != nil {
			return err
		}
		if err := journal.WriteAtomic(result+".second", next); err != nil {
			return err
		}
	}
	_, err = io.Copy(io.Discard, in)
	return err
}

func blockedReadChild(mode, result, session, message string) error {
	if err := journal.WriteAtomic(result+".acked", []byte("yes")); err != nil {
		return err
	}
	waitPermit(filepath.Dir(result), filepath.Base(result)+".send", 5*time.Second)
	if err := writeReadCommand(os.Stdout, session, message); err != nil {
		return err
	}
	if mode == "blocked-pipeline" {
		waitPermit(filepath.Dir(result), filepath.Base(result)+".second", 5*time.Second)
		if err := writeReadCommand(os.Stdout, session, message); err != nil {
			return err
		}
	}
	for {
		fds := []unix.PollFd{{Fd: 0, Events: unix.POLLHUP}}
		_, err := unix.Poll(fds, 100)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		if fds[0].Revents&unix.POLLHUP != 0 {
			return journal.WriteAtomic(result+".eof", []byte("yes"))
		}
	}
}

func TestReadV1RealChildBodyPrivacyAndDeferred(t *testing.T) {
	for _, mode := range []string{"echo-stderr", "happy", "deferred", "rate", "cancel", "request-limit", "audit-unavailable", "raw-over", "encoded-over"} {
		t.Run(mode, func(t *testing.T) {
			testReadBodyScenario(t, mode)
		})
	}
}

func assertReadPrivacy(t *testing.T, x *readFixture, err error, auditUnavailable bool) {
	t.Helper()
	if (err != nil && strings.Contains(err.Error(), "B2-PRIVATE")) || strings.Contains(x.output.String(), "B2-PRIVATE") || strings.Contains(x.logs.String(), "B2-PRIVATE") {
		t.Fatal("host exposed a returned body")
	}
	assertReadMetadataPrivate(t, x.store.Dir)
	raw, readErr := os.ReadFile(filepath.Join(x.store.Dir, "plugin-read-audit.json"))
	if (!auditUnavailable && readErr != nil) || bytes.Contains(raw, []byte("B2-PRIVATE")) {
		t.Fatal("audit absent or exposed body", readErr)
	}
}

func TestReadV1RequiresOptInACKAndBoundedSingleRequest(t *testing.T) {
	for _, mode := range []string{"v0-read", "before-ack", "instead-of-ack", "pipeline", "oversize-request", "injected-binding"} {
		t.Run(mode, func(t *testing.T) {
			x := readHostFixture(t, mode, readTestBody, false)
			_, done := x.start(t)
			result := awaitReadResult(t, x.result)
			if result.Code == "ok" || result.Message != nil {
				t.Fatal("unaccepted child request reached body reader", mode)
			}

			err := awaitReadStop(t, done)
			if err == nil || strings.Contains(err.Error(), readTestBody) || strings.Contains(err.Error(), "FORBIDDEN-CHILD-CLAIM") {
				t.Fatal("protocol failure leaked or succeeded", err)
			}
			if raw, readErr := os.ReadFile(filepath.Join(x.store.Dir, "plugin-read-audit.json")); readErr == nil {
				t.Fatal("unaccepted child request reached body reader", len(raw))
			} else if !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
		})
	}
}

func TestReadV1BlockedPipeRevokeAndPipeline(t *testing.T) {
	for _, mode := range []string{"blocked-revoke", "blocked-pipeline"} {
		t.Run(mode, func(t *testing.T) { testBlockedRead(t, mode) })
	}
}

func testBlockedRead(t *testing.T, mode string) {
	t.Helper()
	x := readHostFixture(t, mode, strings.Repeat("\x01", 5000), false)
	c, done := startReadPipeHost(t, x)
	eventually(t, func() bool { _, err := os.Stat(x.result + ".acked"); return err == nil })
	fillReadPipe(t, c.input)
	if err := journal.WriteAtomic(x.result+".send", []byte("yes")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		raw, err := os.ReadFile(filepath.Join(x.store.Dir, "plugin-read-audit.json"))
		return err == nil && bytes.Contains(raw, []byte(`"outcome": "ok"`))
	})
	select {
	case <-done:
		t.Fatal("response was not blocked by full pipe")
	case <-time.After(30 * time.Millisecond):
	}
	started := time.Now()
	if mode == "blocked-revoke" {
		revoked := make(chan error, 1)
		go func() { revoked <- x.feed.RevokePluginCapabilities() }()
		select {
		case err := <-revoked:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("pipe I/O retained the Store lock")
		}
	} else if err := journal.WriteAtomic(x.result+".second", []byte("yes")); err != nil {
		t.Fatal(err)
	}
	if err := awaitReadStop(t, done); err == nil {
		t.Fatal("blocked write was accepted")
	}
	if elapsed := time.Since(started); elapsed >= 900*time.Millisecond {
		t.Fatal("write ended by its1s deadline instead of binding/pipeline cancellation", elapsed)
	}
	if err := c.stop(errors.New("test observes transport failure")); err == nil {
		t.Fatal("private stop lost failure")
	}
	if _, err := os.Stat(x.result + ".eof"); err != nil {
		t.Fatal("child did not observe EOF", err)
	}
}

func fillReadPipe(t *testing.T, pipe *os.File) {
	t.Helper()
	if err := pipe.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte(" "), 4096)
	for {
		_, err := pipe.Write(chunk)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func readChildTestMain() {
	if len(os.Args) != 6 || os.Args[1] != "read-test-child" {
		return
	}
	if err := readTestChild(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func readChildHandshake(mode, result string) (*bufio.Reader, error) {
	version := "v1-read"
	if mode == "v0-read" {
		version = "v0"
	}
	if err := writeFrame(os.Stdout, frame{Hello: &hello{Version: version}}); err != nil {
		return nil, err
	}
	in := bufio.NewReaderSize(os.Stdin, maxFrame+1)
	if version == "v1-read" {
		confirmation, err := readFrame(in)
		if err != nil || confirmation.Hello == nil || confirmation.Hello.Version != "v1-read" {
			return nil, errors.New("missing v1 confirmation")
		}
	}
	if mode != "before-ack" {
		offered, err := readFrame(in)
		if err != nil {
			return nil, err
		}
		if err := sendReadFixtureACK(mode, result, offered.ID); err != nil {
			return nil, err
		}
	}
	return in, nil
}

func assertReadFixtureResponse(t *testing.T, mode string, result readTestOutcome, body string) {
	t.Helper()

	if mode == "audit-unavailable" || mode == "raw-over" || mode == "encoded-over" {
		expected := "too_large"
		if mode == "audit-unavailable" {
			expected = "audit_unavailable"
		}
		if result.Code != expected || result.Message != nil {
			t.Fatal("read failure exposed a body", mode, result.Code)
		}
		return
	}
	if mode == "deferred" {
		if result.Code != "native_deferred" || result.Message == nil || result.Message.Body != nil {
			t.Fatal("native pending body leaked", result.Code)
		}
	} else if result.Code != "ok" || result.Message == nil || result.Message.Body == nil || *result.Message.Body != body {
		t.Fatal("real child did not read exact body", result.Code)
	}
}

func assertReadMetadataPrivate(t *testing.T, state string) {
	t.Helper()

	for _, dir := range []string{"events", "plugins"} {
		if walkErr := filepath.WalkDir(filepath.Join(state, dir), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			raw, err := os.ReadFile(path)
			if bytes.Contains(raw, []byte("B2-PRIVATE")) {
				t.Error("body leaked to metadata", path)
			}
			return err
		}); walkErr != nil {
			t.Fatal(walkErr)
		}
	}
}

func startReadPipeHost(t *testing.T, x *readFixture) (*child, <-chan error) {
	t.Helper()

	status, err := x.feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	if err := status.Launch.Verify(); err != nil {
		t.Fatal(err)
	}
	lease, err := x.feed.AcquirePluginLease()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	c, err := launchChild(ctx, status.Launch.Executable, status.Launch.Args, status.DataDir, "", lease)
	if err != nil {
		cancel()
		_ = lease.Close()
		t.Fatal(err)
	}
	c.readMode.Store(true)
	t.Cleanup(func() { cancel(); _ = c.stop(nil); _ = lease.Close() })
	header, err := c.receive(2 * time.Second)
	if err != nil || header.Hello == nil || header.Hello.Version != "v1-read" {
		t.Fatal("actual child hello", err)
	}
	done := make(chan error, 1)
	go func() { done <- runReadHost(ctx, x.feed, &status, c, &x.output, "reader ready", header) }()
	return c, done
}

func testReadBodyScenario(t *testing.T, mode string) {
	t.Helper()
	body := readScenarioBody(mode)
	x := readHostFixture(t, mode, body, mode == "deferred")
	if mode == "audit-unavailable" {
		if err := os.Mkdir(filepath.Join(x.store.Dir, "plugin-read-audit.json"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cancel, done := x.start(t)
	result := awaitReadResult(t, x.result)
	assertReadFixtureResponse(t, mode, result, body)
	if _, err := model.UUID(result.Request); err != nil {
		t.Fatal("host request UUID missing", err)
	}
	if mode == "rate" {
		second := awaitReadResult(t, x.result+".second")
		if second.Code != "rate_limited" || second.Message != nil {
			t.Fatal("persistent transport rate failed", second.Code)
		}
	}
	err := finishReadScenario(t, x, mode, cancel, done)
	assertReadPrivacy(t, x, err, mode == "audit-unavailable")
}

func finishReadScenario(t *testing.T, x *readFixture, mode string, cancel context.CancelFunc, done <-chan error) error {
	t.Helper()

	if mode == "cancel" {
		cancel()
	} else if mode != "echo-stderr" {
		if err := x.feed.RevokePluginCapabilities(); err != nil {
			t.Fatal(err)
		}
	}
	err := awaitReadStop(t, done)
	if (mode == "cancel") != (err == nil) {
		t.Fatal("transport cancellation/failure outcome", mode, err)
	}
	return err
}

func readScenarioBody(mode string) string {
	switch mode {
	case "raw-over":
		return strings.Repeat("x", (16<<10)+1)
	case "encoded-over":
		return strings.Repeat("\x01", 6000)
	default:
		return readTestBody
	}
}

func sizedReadFixture(session, message string, limit int) error {
	var b bytes.Buffer
	if err := writeReadCommand(&b, session, message); err != nil {
		return err
	}
	raw := b.Bytes()
	padding := limit - len(raw)
	if padding < 0 {
		return errors.New("fixture request too long")
	}
	raw = append(raw[:len(raw)-1], bytes.Repeat([]byte(" "), padding)...)
	_, err := os.Stdout.Write(append(raw, '\n'))
	return err
}

func TestReadV1MonitorRejectsBindingMutation(t *testing.T) {
	for _, field := range []string{"revision", "pin", "installation", "enabled"} {
		t.Run(field, func(t *testing.T) {
			x := readHostFixture(t, "monitor", readTestBody, false)
			_, done := x.start(t)
			eventually(t, func() bool { status, err := x.feed.PluginStatus(); return err == nil && !status.GapPending })
			rewriteReadBinding(t, x, field)
			if err := awaitReadStop(t, done); err == nil {
				t.Fatal("binding monitor did not terminate", field)
			}
		})
	}
}

func rewriteReadBinding(t *testing.T, x *readFixture, field string) {
	t.Helper()
	path := filepath.Join(x.store.Dir, "plugins", "reader.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	capabilities, ok := record["capabilities"].(map[string]any)
	if !ok {
		t.Fatal("fixture capabilities missing")
	}
	switch field {
	case "revision":
		capabilities["revision"] = "30000000-0000-4000-8000-000000000001"
	case "pin":
		launch, ok := record["launch"].(map[string]any)
		if !ok {
			t.Fatal("fixture launch missing")
		}
		launch["pin_sha256"] = strings.Repeat("a", 64)
		capabilities["pin_sha256"] = launch["pin_sha256"]
	case "installation":
		record["id"] = "30000000-0000-4000-8000-000000000001"
		capabilities["installation"] = record["id"]
	case "enabled":
		record["enabled"] = false
	}
	raw, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(path, raw); err != nil {
		t.Fatal(err)
	}
}

func sendReadFixtureACK(mode, result, id string) error {
	if mode == "instead-of-ack" {
		return nil
	}
	if mode == "ack-sync-cut" {
		if err := journal.WriteAtomic(result+".offer", []byte("yes")); err != nil {
			return err
		}
		waitPermit(filepath.Dir(result), filepath.Base(result)+".ack", 5*time.Second)
	}
	return writeFrame(os.Stdout, frame{Ack: id})
}

func TestReadV1RequiresDurableMetadataACK(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory sync cut requires ordinary user")
	}
	x := readHostFixture(t, "ack-sync-cut", readTestBody, false)
	_, done := x.start(t)
	eventually(t, func() bool { _, err := os.Stat(x.result + ".offer"); return err == nil })
	plugins := filepath.Join(x.store.Dir, "plugins")
	if err := os.Chmod(plugins, 0300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(plugins, 0700) })
	if err := journal.WriteAtomic(x.result+".ack", []byte("yes")); err != nil {
		t.Fatal(err)
	}
	result := awaitReadResult(t, x.result)
	if result.Code == "ok" || result.Message != nil {
		t.Fatal("non-durable metadata ACK released body", result.Code)
	}
	if err := awaitReadStop(t, done); err == nil {
		t.Fatal("non-durable ACK accepted")
	}
	if raw, err := os.ReadFile(filepath.Join(x.store.Dir, "plugin-read-audit.json")); err == nil {
		t.Fatal("non-durable ACK reached body reader", len(raw))
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
