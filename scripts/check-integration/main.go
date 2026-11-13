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

// importPath maps a required package to its import path: "cmd/..." is under
// the module root, everything else under internal/.
func (t requiredTest) importPath() string {
	if strings.HasPrefix(t.pkg, "cmd/") {
		return "github.com/zalkilabs/zalki/" + t.pkg
	}
	return "github.com/zalkilabs/zalki/internal/" + t.pkg
}

var required = []requiredTest{
	{"adapters", "TestCursorPromptRegistersAndArmsResumedChat"},
	{"adapters", "TestCursorStaleHookCannotTouchReplacementGeneration"},
	{"bus", "TestPromptRegistrationConcurrentProcesses"},
	{"bus", "TestPromptRegistrationConcurrentGlobalQuota"},
	{"bus", "TestPromptRegistrationCrashAfterDurableDebit"},
	{"bus", "TestPromptRegistrationWALRecoveryDoesNotRecharge"},
	{"bus", "TestPromptQuotaProcessCrashPublicationCuts"},
	{"adapters", "TestCursorRecoveryCrashAfterClaimDoesNotRetry"},
	{"pluginfeed", "TestPluginOldParentReplyTargetsPromptReplacement"},
	{"cmd/zalki", "TestUpgradeWithoutInstallationRecordCLI"},
	{"cmd/zalki", "TestConfigSetUnsetCLI"},
	{"cmd/zalki", "TestDoctorCodexTrustIsOffline"},
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
	{"adapters", "TestClaudeLivenessAfterKillAndCleanExit"},
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
	{"bus", "TestProcessFingerprintTracksRealExit"},
	{"bus", "TestRememberOwnerFindsItsParentInAControlledChild"},
	{"cmd/zalki", "TestPluginInstallRealTerminalCtrlC"},
	{"cmd/zalki", "TestPluginInstallRealTerminalCtrlCDuringBuild"},
	{"cmd/zalki", "TestPluginInstallRealTerminalSecondCtrlCKills"},
	{"cmd/zalki", "TestPluginInstallSignalCancels"},
	{"cmd/zalki", "TestPluginInstallSecondSignalKills"},
	{"plugins", "TestV2HostRoundTripWithRealChild"},
	{"cmd/zalki", "TestPluginInstallBuildFailureKeepsOnlyLog"},
	{"plugins", "TestRunInstallBuildTurnsGoTelemetryOff"},
	{"plugins", "TestPublishedWireFramesMatchTheHost"},
	{"plugins", "TestHostFramesWithExtraFieldsAreSupersets"},
	{"plugins", "TestWireFixtureValuesArePublished"},
	{"plugins", "TestHostRefusesTheOldWireByName"},
	{"plugins", "TestHostNeverEchoesAnUnknownHelloVersion"},
	{"plugins", "TestGrantedPluginStderrIsWithheldWhenTheHelloFails"},
	{"plugins", "TestHostStopsAPluginThatRequiresAMissingCapability"},
	{"plugins", "TestHostOffersOnlyGrantedCapabilities"},
	{"plugins", "TestV2HostRefusesAnOpTheHelloDidNotEnable"},
	{"model", "TestPublishedPluginEnums"},
	{"plugins", "TestValidPluginID"},
	{"pluginfeed", "TestReceiptWithoutIDIsAnOldRecord"},
	{"pluginfeed", "TestScopeIsAllOrNoneAndFailsClosed"},
	{"pluginfeed", "TestPluginMemberIsALiveSession"},
	{"pluginfeed", "TestFiveChecksUseTheSameMembership"},
	{"pluginfeed", "TestFeedSuppressesUnprovedRichFinalEvents"},
	{"pluginfeed", "TestSessionControlRestrictedFinalEventsNeverEscape"},
	{"bus", "TestSessionPluginRawAckChecksBeforeResultAndMutation"},
	{"bus", "TestSessionPluginRawAckAllowedAndNative"},
	{"bus", "TestSessionPluginBlockedCursorCannotOrderVisibleRows"},
	{"bus", "TestSessionPluginAllowedAndNativeCursorRemainUsable"},
	{"pluginfeed", "TestRevisionCoversOwnerChoicesOnly"},
	{"pluginfeed", "TestMembershipChangesKeepTheBinding"},
	{"pluginfeed", "TestTamperedRevisionIsRefused"},
	{"pluginfeed", "TestNoFeedWithoutStatusAccess"},
	{"pluginfeed", "TestFeedScanOf64SessionsHoldsTheLockBriefly"},
	{"cmd/zalki", "TestPluginInstallRefusesExtendedAccess"},
	{"cmd/zalki", "TestPluginInstallRealTerminalApproves"},
	{"cmd/zalki", "TestPluginInstallRealTerminalCancelKeys"},
	{"cmd/zalki", "TestPluginListUnknownScopeIsSafe"},
	{"cmd/zalki", "TestPluginInstallUnsafeSourceDiagnostics"},
	{"cmd/zalki", "TestPluginInstallSourceErrorClassification"},
	{"pluginfeed", "TestInstallKeepsForeignArtifacts"},
	{"pluginfeed", "TestPluginGrantEnabledRebind"},
	{"pluginfeed", "TestPluginRemoveHeldLease"},
	{"pluginfeed", "TestPluginRebindPublicationUncertainty"},
	{"pluginfeed", "TestPluginPublicationOutcomeIsProvenOrUnknown"},
	{"plugins", "TestPluginRunNamedRefusal"},
	{"plugins", "TestPluginManualRuntimeEnvironment"},
	{"plugins", "TestPluginHomeMustBeCleanAbsolute"},
	{"plugins", "TestPluginPinIdentityAfterStart"},
	{"adapters/codex", "TestCodexSessionEndBoundUpdateAndError"},
	{"adapters", "TestCodexSessionEndKeepsListedAndQueues"},
	{"adapters", "TestCodexSessionEndPreservesQueueOptIn"},
	{"adapters", "TestCodexSessionEndPreservesPendingMessages"},
	{"adapters", "TestOtherSessionEndStillRetires"},
	{"adapters", "TestCodexSessionStartAfterEndKeepsActorID"},
	{"adapters", "TestCodexSessionEndRestartBurstKeepsAllSessions"},
	{"adapters", "TestPruneCodexNeedsEvidenceOrExplicitID"},
	{"adapters", "TestCodexSessionEndLeavesHeldUntilNormalRelease"},
	{"cmd/zalki", "TestPluginUpdateCheckExitCodes"},
	{"cmd/zalki", "TestPluginInstallDryRun"},
	{"plugins", "TestInstallBuildReapedLeaderNeverSignalsGroup"},
	{"plugins", "TestInstallBuildDirectKillPrecedesWait"},
	{"plugins", "TestInstallBuildIdentityLossSuppressesGroup"},
	{"plugins", "TestInstallBuildCancellationKeepsUnreapedAuthority"},
	{"plugins", "TestInstallBuildIdentityChangesRefuseSignal"},
	{"plugins", "TestInstallBuildFixturePIDNeverSignals"},
	{"plugins", "TestInstallBuildOutputFailureStopsBeforeWait"},
	{"plugins", "TestInstallBuildCanceledBeforeStart"},
	{"plugins", "TestInstallBuildFinishNeverSignalsAfterReaping"},
	{"cmd/zalki", "TestPTYInstallCleanupNeverSignalsReapedLeader"},
	{"cmd/zalki", "TestGatedBuildCleanupNeverSignalsRecordedGroup"},
	{"cmd/zalki", "TestPTYInstallCleanupStopsUnreapedExitedLeaderGroup"},
	{"plugins", "TestForegroundFixtureNeverSignalsAfterWaitStarts"},
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
			if event.Package != test.importPath() {
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

// Alias mode adds the in-process proof without copying the current root list.
func requiredForMode(alias bool) []requiredTest {
	tests := append([]requiredTest(nil), required...)
	if alias {
		tests = append(tests, requiredTest{"cmd/zalki", "TestTempAliasEnvironment"})
	}
	return tests
}

func main() {
	alias, path, err := reportArguments(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	tests := requiredForMode(alias)
	if err := runRequired(path, tests); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Required integration coverage passed (%d tests, no skipped descendants).\n", len(tests))
}

func reportArguments(args []string) (bool, string, error) {
	if len(args) == 1 && args[0] != "--alias" {
		return false, args[0], nil
	}
	if len(args) == 2 && args[0] == "--alias" {
		return true, args[1], nil
	}
	return false, "", errors.New("usage: check-integration [--alias] GO_TEST_JSON")
}

func runRequired(path string, tests []requiredTest) error {
	f, err := os.Open(path) //nolint:gosec // G304: explicit local test report supplied by the operator.
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // Read-only test report.
	return check(f, tests)
}
