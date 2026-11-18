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
// the module root, as are scripts; everything else is under internal/.
func (t requiredTest) importPath() string {
	if strings.HasPrefix(t.pkg, "cmd/") || strings.HasPrefix(t.pkg, "scripts/") {
		return "github.com/zalkilabs/zalki/" + t.pkg
	}
	return "github.com/zalkilabs/zalki/internal/" + t.pkg
}

var required = []requiredTest{
	{"scripts/assertion-sites", "TestMutationValuesStayInsideNamedPackageDeclaration"},
	{"scripts/assertion-sites", "TestMutationValueBoundsRejectAdjacentAndWrongKinds"},
	{"scripts/assertion-sites", "TestNamedCallbackLineageUsesItsOwnFatal"},
	{"scripts/assertion-sites", "TestNamedCallbackLineageRejectsHelperOrAmbiguousReferences"},
	{"scripts/assertion-sites", "TestNamedCallbackLineageRejectsWrongReceiverAndMixedEdges"},
	{"scripts/check-integration", "TestRequiredJSONPreservesTheCurrentOrdinaryRootLedger"},
	{"scripts/assertion-sites", "TestAssertionSitesResolveCurrentHelperAndDescendantSource"},
	{"scripts/assertion-sites", "TestAssertionSitesRejectExtraRPCBranchAndMissingHelper"},
	{"scripts/assertion-sites", "TestAssertionSitesRefuseDisconnectedOrAmbiguousCallers"},
	{"scripts/assertion-sites", "TestAssertionSitesRejectSourceLinksAndMetadata"},
	{"scripts/assertion-sites", "TestAssertionSitesBindTheWholeQuotedFatalCall"},
	{"scripts/assertion-sites", "TestAssertionSitesFreshTestingCallbackReportsItsOwnFatal"},
	{"scripts/assertion-sites", "TestAssertionMutationPreimageStaysInsideItsNamedFunction"},
	{"scripts/arch-metrics", "TestSnapshotShapeIgnoresDeclarationMovement"},
	{"scripts/arch-metrics", "TestSnapshotRejectsPartialDiscovery"},
	{"scripts/arch-metrics", "TestSnapshotUsesTheRepoNamedExportRule"},
	{"scripts/arch-metrics", "TestSnapshotUsesSelectedFilesAndRefusesDuplicateDeclarations"},
	{"scripts/arch-metrics", "TestSnapshotRetainsAliasReceiverAndWireShapeChanges"},
	{"scripts/arch-metrics", "TestSnapshotNeverReadsLinkedOutsideSource"},
	{"scripts/arch-metrics", "TestSnapshotRetainsImplicitConstantGroupContext"},
	{"scripts/arch-metrics", "TestSnapshotRawOSExceptionsAreExactNotWildcardBudgets"},
	{"scripts/issue-scopes", "TestStrictScopeRejectsSourceSymlinks"},
	{"scripts/check-integration", "TestRequiredIntegrationGateScriptPackage"},
	{"scripts/issue-scopes", "TestLiteralScopeSurvivesLineMovement"},
	{"scripts/issue-scopes", "TestStrictReportRequiresAnExplicitIssueCollection"},
	{"scripts/issue-scopes", "TestStrictScopeRejectsMissingEscapedAndOutOfBoundsSource"},
	{"bus", "TestEndpointRecordsTheResolvedPath"},
	{"bus", "TestEndpointRejectsFilesAndSymlinks"},
	{"bus", "TestReleaseLinkerFlagNamesTheVersionVariable"},
	{"bus", "TestFeedMemberZeroReferenceCacheCannotBeRevived"},
	{"bus", "TestFeedMemberReadsAndDecodesNativeOncePerView"},
	{"bus", "TestFeedMemberEvidenceIsOncePerViewAndPluginIsolated"},
	{"bus", "TestFeedMemberCacheInvalidatesOnNativeWrite"},
	{"bus", "TestFeedMemberCacheDoesNotShareEvidenceAcrossRoots"},
	{"pluginfeed", "TestPluginInstallEnabledRecordHasEnableIncarnation"},
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
	{"adapters", "TestCodexPermissionRequestDoesNotHoldAnIdleSend"},
	{"adapters", "TestClaudePermissionRequestStillHoldsUntilStop"},
	{"adapters", "TestCodexTransportHoldNonCompactStartKeepsSettlementPolicy"},
	{"adapters/codex", "TestCodexLiveApprovalHoldsThroughStopAndEnd"},
	{"adapters/codex", "TestCodexApprovalRpcRejectionIsQueued"},
	{"adapters/codex", "TestCodexLateNativeApprovalPreservesDirectFinalRefusal"},
	{"adapters/codex", "TestCodexPermissionHookWithIdleLiveThreadDelivers"},
	{"bus", "TestApprovalLatePublicationErrorKeepsCountAndReleases"},
	{"bus", "TestApprovalPendingIntentErrorKeepsCountUntilRecovery"},
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
	{"mcpserver", "TestMCPUncertainSendHistoryAndAck"},
	{"cmd/zalki", "TestSendCommandProjectsStoredUncertainty"},
	{"adapters/codex", "TestCodexUncertainDeliveryNeverReplays"},
	{"cmd/zalki", "TestPTYInstallCleanupStopsUnreapedExitedLeaderGroup"},
	{"plugins", "TestForegroundFixtureNeverSignalsAfterWaitStarts"},
	{"bus", "TestLeaseOperationsAndPruneDoNotDeadlock"},
	{"bus", "TestEveryHookStageIsLoggable"},
	{"bus", "TestHookStageInventoryRejectsDynamicForms"},
	{"cmd/zalki", "TestPluginSupervisorRealChildrenCLI"},
	{"cmd/zalki", "TestPluginSupervisorForegroundAndStopCLI"},
	{"bus", "TestRuntimeActorUpdateSurvivesSchemaGuard"},
	{"bus", "TestRuntimeRecordHeaderIsPrivateAndExact"},
	{"bus", "TestMixedRuntimeHeadersKeepDoctorAndPruneAvailable"},
	{"bus", "TestObsoleteSessionBackfillsAreRefused"},
	{"adapters", "TestCodexUnlabeledPromptFilterSemantics"},
	{"cmd/zalki", "TestOldSessionGuidanceKeepsHealthyRows"},
	{"bus", "TestRegistrationRequiresAbsoluteNativeProfile"},
	{"bus", "TestStoredNativeProfileRefusalKeepsAuthorityAndMail"},
	{"bus", "TestCompactionCannotChangeNativeProfile"},
	{"bus", "TestCompactionComparesProfileAfterTakingJournalLock"},
	{"adapters", "TestNativeProfileSelectionIsAbsoluteAndExact"},
	{"adapters", "TestNativeProfileCreationRoutes"},
	{"adapters", "TestCursorPromptProfileRefusalDoesNotDebitQuota"},
	{"adapters", "TestNativeNameLookupNeverUsesForeignProfile"},
	{"adapters/codex", "TestCodexBoundProfileNeverUsesSenderRPC"},
	{"adapters/codex", "TestCodexBoundQueueRequiresCapturedProfile"},
	{"adapters/codex", "TestCodexUnboundProfileProbeRemainsReadOnly"},
	{"cmd/zalki", "TestPluginLoginItemConsentCLI"},
	{"cmd/zalki", "TestPluginLoginItemDecisionTable"},
	{"cmd/zalki", "TestPluginLoginItemRecheckAfterBuild"},
	{"cmd/zalki", "TestPluginLoginItemSelectorProvenance"},
	{"install", "TestPluginLoginItemRelaunchCrashPoints"},
	{"install", "TestPluginLoginItemCooperatingProcess"},
	{"cmd/zalki", "TestPluginRuntimeStatusPrivacy"},
	{"cmd/zalki", "TestPluginRuntimeListLines"},
	{"cmd/zalki", "TestPluginBackgroundDoctor"},
	{"cmd/zalki", "TestPluginRunStateHostStart"},
	{"cmd/zalki", "TestPluginRuntimeObservedRunning"},
	{"cmd/zalki", "TestPluginSupervisorLockSurvivesStatusProbe"},
	{"cmd/zalki", "TestPluginBackgroundInstalledBy"},
	{"cmd/zalki", "TestPluginFirstStartNotificationBestEffort"},
	{"cmd/zalki", "TestPluginNotificationBudget"},
	{"cmd/zalki", "TestPluginNotificationInFlightFence"},
	{"cmd/zalki", "TestPluginNotificationLines"},
	{"cmd/zalki", "TestFirstStartNotificationNeverRepeats"},
	{"cmd/zalki", "TestPluginEnableResetsCrashLimit"},
	{"install", "TestPluginLoginItemRealLaunchctlOutputs"},
	{"cmd/zalki", "TestPluginEnableWakesSupervisor"},
	{"cmd/zalki", "TestPluginLogFailureWaits"},
	{"cmd/zalki", "TestPluginSupervisorLeaseHeldIsNotACrash"},
	{"cmd/zalki", "TestPluginInstallLineWithRunningSupervisor"},
	{"cmd/zalki", "TestPluginNotificationCustomStateSendsNothing"},
	{"cmd/zalki", "TestPluginLoginItemPendingNamesRepair"},
	{"cmd/zalki", "TestPluginGroupGraceExceedsPluginGrace"},
	{"cmd/zalki", "TestPluginSuperviseStateFollowsEnvironment"},
	{"cmd/zalki", "TestPluginSuperviseEnvironmentSelectionCLI"},
	{"plugins", "TestHostDisableEndsQuietlyWhenPluginExitsNonZero"},
	{"cmd/zalki", "TestPluginSupervisorUnknownSnapshotAtExitIsNotACrash"},
	{"cmd/zalki", "TestPluginReinstallIgnoresLeftoverRunFiles"},
	{"cmd/zalki", "TestPluginShowAndListPrintAddress"},
	{"cmd/zalki", "TestPluginSupervisorForgetsRemovedNames"},
	{"scripts/assertion-sites", "TestBundleCurrentAssertionsBindReviewedWholeRootSites"},
	{"scripts/assertion-sites", "TestBundleLiteralArgumentsKeepDistinctSiblingReports"},
	{"scripts/assertion-sites", "TestBundleLiteralArgumentsRejectWrongRequestBindings"},
	{"scripts/assertion-sites", "TestBundleLiteralArgumentsRejectChangedSourceBoundaries"},
	{"scripts/assertion-sites", "TestBundleRegisteredPrefixesRejectWrongOrDisconnectedPaths"},
	{"cmd/zalki", "TestStateArchiveCollisionNeverOverwrites"},
	{"cmd/zalki", "TestStateArchiveRefusesChangedSourceAfterConsent"},
	{"cmd/zalki", "TestStateArchiveMissingOrUnsafeLockIsNeverCreated"},
	{"cmd/zalki", "TestStateArchiveProbeFailureRemainsClosed"},
	{"cmd/zalki", "TestStateArchiveRenameAndSyncOutcomesAreHonest"},
	{"cmd/zalki", "TestStateArchiveBackgroundEvidenceIsConservative"},
	{"cmd/zalki", "TestStateArchiveUnsafeRootsAndIdentityProbesRefuse"},
	{"cmd/zalki", "TestStateArchiveMovesOpaqueBytesAndAsksOnce"},
	{"cmd/zalki", "TestStateArchiveDeclineAndEOFDoNotReserveAnything"},
	{"cmd/zalki", "TestStateArchiveMissingStateNeverInitializesLogging"},
	{"cmd/zalki", "TestStateArchiveRefusesHeldLocksWithoutMoving"},
	{"cmd/zalki", "TestStateArchiveKeepsJournalAndSupervisorLocksThroughMove"},
	{"cmd/zalki", "TestStateArchiveRefusesPostConsentActivity"},
	{"cmd/zalki", "TestStateArchiveRefusesLiveProcessAndSocket"},
	{"bus", "TestStoreArchiveWaiterRefusesBeforeFreshRecovery"},
	{"cmd/zalki", "TestPluginRuntimePublicationUsesCurrentParentPath"},
	{"pluginfeed", "TestPluginAccessMessagesClassifiesEveryField"},
	{"plugins", "TestHostStderrUsesMessageAccess"},
	{"bus", "TestCodexPostToolUseStreamingSelection"},
	{"bus", "TestCodexPostToolUseSelectorRefusesInvalid"},
	{"bus", "TestCodexPostToolUseLargeApprovalLifecycle"},
	{"bus", "TestCodexPostToolUseInvalidCannotReleaseApproval"},
	{"bus", "TestCodexPostToolUseStreamingFraming"},
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
	if len(os.Args) == 2 && os.Args[1] == "--list-required" {
		if err := writeRequiredJSON(os.Stdout, requiredForMode(false)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
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

func writeRequiredJSON(out io.Writer, tests []requiredTest) error {
	rows := make([]map[string]string, 0, len(tests))
	for _, test := range tests {
		rows = append(rows, map[string]string{"package": test.importPath(), "test": test.name})
	}
	return json.NewEncoder(out).Encode(rows)
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
