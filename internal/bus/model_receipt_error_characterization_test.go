package bus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"syscall"
	"testing"
	"time"

	agentapi "github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

var modelReceiptTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{9}Z$`)

func requireModelTimestamp(t *testing.T, at string) {
	t.Helper()
	if !modelReceiptTimestamp.MatchString(at) {
		t.Fatalf("receipt timestamp %q, want UTC with exactly nine fractional digits", at)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000000000Z", at); err != nil {
		t.Fatalf("receipt timestamp is not a valid time: %v", err)
	}
}

func TestModelNewReceiptBaseline(t *testing.T) {
	receipt := bus.NewReceipt(model.StatusUnknownWrite)
	requireModelTimestamp(t, receipt.At)
	// Normalize only the clock sample; every other output remains a literal.
	receipt.At = "TIME"
	requireModelJSON(t, receipt, `{"at":"TIME","status":"unknown_after_write"}`)
}

func transportCauseKind(t *testing.T, receipt bus.Receipt) string {
	t.Helper()
	target := nativeAdapter("beta")
	target.script(func(context.Context, bus.Message, bus.Session) bus.Receipt { return receipt })
	store := newStore(t, nativeAdapter("alpha"), target)
	from, to := register(t, store, "alpha"), register(t, store, "beta")
	var logs bytes.Buffer
	store.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	if _, err := (&bus.Service{Store: store}).Send(t.Context(), bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "body"}); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&logs)
	for {
		var row map[string]any
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			t.Fatal("transport.completed log was not recorded")
		} else if err != nil {
			t.Fatal(err)
		}
		if row["msg"] == "transport.completed" {
			kind, _ := row["error_kind"].(string)
			return kind
		}
	}
}

func TestModelReceiptCauseIsCopiedAndNotEncoded(t *testing.T) {
	original := bus.Receipt{At: "at", Status: model.StatusSubmitted, Error: "public error", Output: "output"}
	withCause := original.WithCause(fmt.Errorf("private diagnostic: %w", syscall.ECONNREFUSED))
	const want = `{"at":"at","status":"submitted","error":"public error","output":"output"}`
	requireModelJSON(t, original, want)
	requireModelJSON(t, withCause, want)
	// Observe the private cause through public transport logging, rather than a
	// private field read or the Cause accessor planned for the later type move.
	if got := transportCauseKind(t, original); got != "" {
		t.Fatalf("WithCause modified its original value: error_kind=%q", got)
	}
	if got := transportCauseKind(t, withCause); got != "connection_refused" {
		t.Fatalf("copied receipt lost its live cause: error_kind=%q", got)
	}
}

func TestModelErrorClassificationBaseline(t *testing.T) {
	coded := &bus.Error{Code: "conflict", Message: "coded refusal"}
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"unknown", errors.New("unknown"), "internal"},
		{"typed", coded, "conflict"},
		{"wrapped typed", fmt.Errorf("outer: %w", coded), "conflict"},
		{"wrapped not found", fmt.Errorf("outer: %w", os.ErrNotExist), "not_found"},
		{"wrapped permission", fmt.Errorf("outer: %w", os.ErrPermission), "unauthorized"},
		{"coded wins joined OS error", errors.Join(os.ErrNotExist, coded), "conflict"},
		{"not found wins joined permission", errors.Join(os.ErrPermission, os.ErrNotExist), "not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := string(bus.ErrorCodeOf(test.err)); got != test.want {
				t.Fatalf("ErrorCodeOf(%v)=%q, want %q", test.err, got, test.want)
			}
		})
	}
}

func TestModelTypedErrorTextAndIdentity(t *testing.T) {
	coded := &bus.Error{Code: "invalid_input", Message: "bad value 7"}
	wrapped := fmt.Errorf("outer: %w", coded)
	if got := coded.Error(); got != "invalid_input: bad value 7" {
		t.Fatalf("typed error text=%q", got)
	}
	if got := wrapped.Error(); got != "outer: invalid_input: bad value 7" {
		t.Fatalf("wrapped error text=%q", got)
	}
	_, publicError := bus.UUID("malformed")
	if publicError == nil || publicError.Error() != "invalid_identity: expected an exact session/message UUID" {
		t.Fatalf("public typed error text=%v", publicError)
	}
	var direct *bus.Error
	if !errors.As(publicError, &direct) || direct.Code != "invalid_identity" {
		t.Fatal("public operation did not return a bus.Error-compatible value")
	}
	var recovered *bus.Error
	if !errors.As(fmt.Errorf("outer: %w", publicError), &recovered) || recovered != direct {
		t.Fatal("wrapped public error lost its typed pointer identity")
	}
}

func TestModelFormattedPublicError(t *testing.T) {
	registry, err := agentapi.NewRegistry(nativeAdapter("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Lookup("missing")
	if err == nil || err.Error() != `invalid_input: unsupported agent "missing"` {
		t.Fatalf("formatted public error=%v", err)
	}
}
