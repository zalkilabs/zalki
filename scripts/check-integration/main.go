// Command check-integration verifies required coverage in uncached go test -json
// output. Local make test may skip unavailable sockets; CI may not.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type requiredTest struct{ pkg, name string }

var required = []requiredTest{
	{"adapters/codex", "TestCodexTransportPinsUUIDAndNeverUsesShell"},
	{"adapters/codex", "TestCodexQueueUsesRecipientProfile"},
	{"adapters/codex", "TestCodexControlSocketAdvertisesDirectDelivery"},
	{"adapters/codex", "TestCodexAppServerDeliveryModes"},
	{"adapters", "TestCodexPromptFiltersOnlyCompletedExactEnvelopes"},
	{"adapters", "TestCodexPromptDoesNotReplayPreviousRegistration"},
	{"adapters", "TestCodexCompactionPreservesQueuedMessageRegistration"},
	{"adapters", "TestCodexStartupAndResumeStillReplaceRegistration"},
	{"adapters", "TestCodexCompactionDoesNotReviveRetiredRegistration"},
	{"adapters/claude", "TestRealClaudeSocketFrame"},
	{"adapters", "TestClaudeRegistrationIdentityAndHealth"},
	{"adapters/claude", "TestClaudeFrameSizeAndMissingEndpoint"},
	{"adapters", "TestClaudeServiceWritesOnceAndPreservesSenderLabel"},
	{"adapters/cursor", "TestCursorSenderRequiresExactHintEcho"},
	{"adapters", "TestCursorWakeListenerAndDelivery"},
	{"adapters", "TestCursorRecoveryRearmsAfterListenerShutdown"},
	{"adapters/cursor", "TestCursorSocketHonorsContext"},
	{"adapters/claude", "TestClaudeWriteHonorsContext"},
	{"adapters/claude", "TestClaudeCanceledBeforeDialDoesNotSend"},
	{"bus", "TestLiveSmokeRunnerWithEmulatedClient"},
}

func check(input io.Reader, tests []requiredTest) error {
	decoder := json.NewDecoder(input)
	ran, passed := map[requiredTest]bool{}, map[requiredTest]bool{}
	for {
		var event struct{ Action, Package, Test string }
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("invalid go test JSON: %w", err)
		}
		for _, test := range tests {
			if event.Package != "github.com/zalkilabs/zalki/internal/"+test.pkg {
				continue
			}
			if event.Action == "skip" && (event.Test == test.name || strings.HasPrefix(event.Test, test.name+"/")) {
				return fmt.Errorf("required integration skipped: %s/%s", test.pkg, event.Test)
			}
			if event.Test == test.name {
				switch event.Action {
				case "run":
					ran[test] = true
				case "pass":
					passed[test] = true
				case "fail":
					return fmt.Errorf("required integration failed: %s/%s", test.pkg, test.name)
				}
			}
		}
	}
	for _, test := range tests {
		if !ran[test] || !passed[test] {
			return fmt.Errorf("required integration missing run/pass: %s/%s", test.pkg, test.name)
		}
	}
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: check-integration GO_TEST_JSON")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Required integration coverage passed (%d tests, no skipped descendants).\n", len(required))
}

func run(path string) error {
	f, err := os.Open(path) //nolint:gosec // G304: explicit local test report supplied by the operator.
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // Read-only test report.
	return check(f, required)
}
