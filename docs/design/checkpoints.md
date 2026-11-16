# Architecture checkpoints

The structural refactor proposed in [architecture PR #26](https://github.com/zalkilabs/zalki/pull/26) is long-running.
A checkpoint is a deliberate pause to confirm we are not accumulating mess and
still follow best practice. This file defines the routine and keeps the log.

## When

After every three to four merged migration PRs, at the end of each phase, and
before any change that opens a new trust boundary (plugins, managed jobs).

## What

1. **Metrics** (`make arch-metrics`, report; fails on a boundary violation):
   package sizes, exported declarations, `Store` breadth, functions over 60
   physical lines and the internal import graph, followed by the canonical
   `.golangci.yml` depguard check. The Go command only reports metrics; it has
   no separate boundary-rule engine. Run `make lint-strict` for cognitive and
   cyclomatic complexity and other API-hygiene findings. That target uses the
   same configuration with extra linters enabled: findings return zero, but
   tool, configuration and analysis failures still fail the command.
2. **Coverage** (`go test -cover ./...`), recorded when the machine is quiet.
   Timing-sensitive tests flake under parallel load.
3. **Independent review** against the checklist below, by someone who did not
   write the changes.
4. **Summary** for the owner: what improved, what did not, what to do next.

## Checklist

- Do the import edges match the target in the architecture proposal, with no new cycles?
- Is every temporary bridge (alias, forwarding wrapper, migration helper) listed
  below with the step that removes it?
- Did every moved behavior keep the characterization suites passing unchanged?
- Did exported surface, `Store` breadth, long functions or complexity grow?
- Do new exported names have doc comments, and new packages a package doc?
- Are `depguard` rules in the same commit as the import they allow?
- Are docs current (the architecture proposal, value-ownership.md, this log)?
- Are `thelper` findings triaged by purpose? Fixture helpers should identify
  themselves with `t.Helper()`; case-specific assertion callbacks should retain
  useful failure locations rather than being marked just to silence lint.
- Are known behaviors we chose not to fix recorded as separate decisions, not
  silently frozen into tests?

## Temporary scaffolding

| Bridge or exception | Current disposition | Removal or reconsideration |
| --- | --- | --- |
| `type processInfo = procwatch.Info` | Removed: `internal/bus/process.go` uses `procwatch.Info` directly | Step 5 removal criterion met in the current source; no alias remains |
| Done-aware forwarding source wrapper | Removed: `internal/bus/exit_source.go` is absent; the observer uses `procwatch.Source` directly | Step 5 removal criterion met; raw ownership is in `internal/procwatch/source*.go` |
| Codex build-version dependency | Retained: `codex_app_server.go` reads `bus.Version`; the exact file is exempted by `client-bus` depguard | Supply the build version from composition in a separately reviewed change, preserving release linker identity |
| Installer-helper exception for clients | Retained approved leaf dependency: `install.HookConfig`, `HookCommand`, `MCPConfig`, `ShellQuote` | Reconsider helper extraction separately before claiming the literal clients-only-`agent`/`model` target; do not duplicate builders or hide imports behind aliases |

These dispositions describe the source at `d47eef92a1712bf40029475a2fcd12e8adfa0db4`.
The earlier "before step 6b" helper-decision timing has passed; retaining that
explicit exception is the current decision, not evidence that its import is gone.
Any new compatibility bridge must be listed with its removal step.

Permanent internal seams are separate: `Store.registry`, `processLiveness` /
`probeProcess` reader injection, `hookRuntime.Listener`, `newID` forwarding to
`model.NewUUID`, and `orderEventDirectory` forwarding to `journal.OrderDirectory`
serve registry access, evidence translation or fault injection. Their presence
does not create a public migration alias or require deletion in this change.
The core transaction/IO boundary, consumer ports, neutral configuration
conversion and injected pluginfeed admission/reservation are retained design.
The canonical raw-OS exception inventory in `.golangci.yml` remains explicit
debt to shrink through reviewed platform extraction; its current selected count
is not a license to add unrelated users. See the current ownership supplement
in [value-ownership.md](value-ownership.md#current-implementation-status).

## Log

### Checkpoint 1: main at 8e1fb80 (after steps 0, 1, 2, 3a, 4a)

Static analysis only; coverage was not re-measured. The checkpoint revision is
`8e1fb804549417bdf8f0a30a3d59b9cfb111681c`. Structural counts were reproduced
with `go run ./scripts/arch-metrics -C <checkout-at-checkpoint>` on **darwin/arm64,
Go 1.26.10, CGO_ENABLED=1**, with default build tags. Counts cover the Go and cgo
production files selected by `go list ./...` under `cmd/` and `internal/`;
other platform files and `scripts/` are excluded. Exported declarations count
top-level functions, methods (including methods on unexported receiver types),
types, constants and variables by name; fields and interface members are
excluded. These are not comparable with earlier ad-hoc counts.

| Measure | Value |
| --- | --- |
| `internal/bus` production files / physical lines | 43 / 6220 |
| `Store` methods (files declaring them) | 99 (26) |
| Exported declarations in `bus` | 188 |
| Import cycles | none (`go list` succeeded) |
| New leaf packages | `procwatch` (stdlib + x/sys), `journal` (fsutil) |
| `bus` files importing `install` | 1 (`adapter_config.go`), removed by step 4b |
| `bus` functions over 60 physical lines | 11 |

### Reproducible lint comparison

The before revision is **`595c33422e4be1fdf7c7143a08dc50f9acae2d26`** (after
feature PRs #19–21, before #22); the after revision is the checkpoint SHA above.
Both were independently re-run with **golangci-lint v2.8.0**, the same canonical
configuration and checkpoint-only settings, the build environment above, and
`run --config <absolute-checkpoint-config> --enable <checkpoint-linters>
--issues-exit-code 0 ./cmd/... ./internal/... ./scripts/...` in each checkout.
The checkpoint linter list is the exact comma-separated `--enable` argument in
`make lint-strict`. Thresholds are gocognit >20, gocyclo 15 minimum, and funlen
80 lines / 50 statements. JSON output was classified as production when its
filename did not end in `_test.go`; each number counts reported findings, not
complexity points or a count of defects. Production includes `scripts/` for
lint, although the structural report excludes it.

| Production finding | Before | Checkpoint 1 |
| --- | ---: | ---: |
| Total | 55 | 52 |
| gocognit | 34 | 29 |
| gocyclo | 3 | 4 |
| exhaustive | 8 | 9 |
| nilerr | 4 | 4 |
| perfsprint | 2 | 2 |
| funlen, gocritic, nakedret, noctx | 1 each | 1 each |
| depguard | 0 | 0 |

Eight previous cognitive-complexity findings are no longer reported:
`ReadMessages` (81), `mergeRecordValue` (78), `prepareMessage` (56),
`writeRecord` (35), `deliverReserved` (32), `Diagnostics` (29),
`newUpgradeCommand` (24), and `newSetupCommand` (21). Three new findings are
reported on functions introduced by splitting larger functions: `scanMessages`
(27), `mergeStruct` (23), and `reserveNewMessage` (24). These are measured
improvements in the touched functions, with remaining complexity debt; the
number of findings is not an aggregate complexity score.

Findings and follow-up decisions:

- Structure improved: leaves exist and satisfy the current depguard rules, and
  the Cursor listener no longer depends on the concrete store. Characterization
  assertions guarded the moves, with necessary type and composition plumbing
  adapted separately.
- The worst unchanged cognitive-complexity findings are `removeConfig` (56),
  `mergeConfig` (54), `Store.prepareReply` (47), `reconcileApprovalHolds` (43),
  `argumentHints` (34), and `exitObserver.sync` (31). A dedicated behavior
  preserving complexity pass should follow the lock and configuration moves,
  kept separate from them so each PR stays reviewable.
- Test `thelper` findings rose from 4 to 54: 50 are in new characterization and
  journal tests. This is a triage queue, not 50 established helper defects.
  Callbacks containing the case's own assertions can intentionally retain their
  assertion locations; fixture helpers can use `t.Helper()`. Parameters unused
  by an individual callback may be named `_` when its shared signature needs T.
- The production `noctx` finding is the Homebrew `exec.Command` in
  `cmd/zalki/upgrade.go`, introduced by #21. It does not propagate command-context
  cancellation; review that separately rather than claim this checkpoint proves
  all Ctrl-C behavior.
- `Store` breadth is unchanged until model and rule extraction (step 5).
- Step 4b's neutral configuration declaration will remove the core-to-installer
  edge. Its lexical validator moves to `fsutil`; before moving the registry in
  step 6, decide whether the target package may depend on that domain-free
  helper or whether validation moves with the registry. Do not claim the final
  target import diagram is already achieved.
- The architecture proposal ([PR #26](https://github.com/zalkilabs/zalki/pull/26))
  was not merged at this checkpoint; the link intentionally targets that PR.
- Coverage was not measured in this checkpoint. Earlier coverage estimates are
  omitted because their exact revision, build selection and command were not
  recorded here. Measure a comparable baseline on a quiet machine.

### Checkpoint 2: main at 60e5e87 (after steps 3b and 4b and the installer complexity pass)

Static analysis only, at `60e5e87f1292d3cd786b30cad181aee726f647e4`, on
**darwin/arm64, Go 1.26.10, CGO_ENABLED=1**, with default build tags. The pinned
linter, enabled linters, thresholds and file selection match checkpoint 1;
canonical dependency rules now reflect the merged package moves. Structural
counts use the same `arch-metrics` selection described above. The [main CI run](https://github.com/zalkilabs/zalki/actions/runs/42255740018)
completed successfully. Coverage was not re-measured.

| Measure | Before refactor (595c334) | Checkpoint 1 (8e1fb80) | Checkpoint 2 (60e5e87) |
| --- | ---: | ---: | ---: |
| Production strict-lint findings | 55 | 52 | 47 |
| gocognit | 34 | 29 | 25 |
| gocyclo | 3 | 4 | 4 |
| noctx | 1 | 1 | 0 |
| exhaustive / nilerr | 8 / 4 | 9 / 4 | 9 / 4 |
| Test `thelper` findings | 4 | 54 | 9 |
| `bus` production files / physical lines | n/a | 43 / 6220 | 43 / 6212 |
| `Store` methods (files) | n/a | 99 (26) | 99 (26) |
| Exported declarations in `bus` | n/a | 188 | 188 |

Findings:

- Since checkpoint 1 the
  installer functions `removeConfig` (56), `mergeConfig` (54),
  `applyConfigEdits` (28) and `validateInstallationPatch` (24) left the list,
  each reduced to 6 or less, with no new findings introduced.
- Boundaries: `bus` has no raw file-lock or flock calls left (they live in
  `journal`), no `bus` file imports `install`, and the depguard rules report
  nothing.
- Test hygiene recovered from 54 to 9 `thelper` findings; the 9 left are the
  assertion-bearing `check` closures in `internal/journal/codec_test.go`, kept
  unmarked on purpose so failures point at the real assertion.
- The `noctx` finding is resolved: command cancellation reaches the Homebrew
  subprocess, and pipe waiting is bounded. This does not establish cancellation
  of all descendant processes.
- Remaining production complexity (gocognit): `Store.prepareReply` 47,
  `reconcileApprovalHolds` 43, `argumentHints` 34, `exitObserver.sync` 31,
  `pruneRun.sweepSessions` 30, then `floodState.admit`, `deleteOrphanFlood` and
  `EmitPending` at 28. Each has its characterization gaps measured and a
  test-first plan; these refactors land before the type moves because they touch
  the same files.
- `Store` breadth and `bus` exported declarations are unchanged and will not
  move until step 5 (model types) and step 6 (agent contract).

Known behaviors found by characterization and deliberately left unchanged,
each open as a separate decision. The installer and reply items are pinned by
tests merged on main; the prune, flood, hook, read and log items are pinned by
pending, unmerged characterization work at this checkpoint. Tests
label them "characterized, not endorsed" so they do not read as promises:

- Installer: an emptied hook event stays as an empty list; a settings file that
  only held our entry is re-indented to `{}`; a whitespace-only settings file is
  a syntax error while an empty file is an empty object; the first pass of
  `applyConfigEdits` can leave a backup and parent directories behind when a
  later edit is refused; validation of a patch with faults in two different
  keys reports one of them in unspecified map order.
- Prune: releasing a listener lease refreshes the updated time of a retired
  session still within the current retention window, extending its retention.
  Sessions already expired under that window are deleted before lease release.
- Flood: on the exact-retry and conflict paths admission returns before
  validating later arrivals.
- Replies: a refused reply still acknowledges the parent message.
- Hooks: for an unreachable approval target, `EmitPending` returns an empty
  context with no error and settles queued approvals.
- Reads: a malformed message record returns an error together with the views
  already collected, in scan order before sorting.
- Logs: `CodeRateLimited` is absent from the event error-code allowlist, so
  rate-limit errors are logged as `internal`. The existing `exhaustive` finding
  identifies this behavior. A log-only correction is tracked separately;
  alias-related warning changes do not demonstrate a behavior fix.

Outstanding architecture work, not yet planned in detail: extracting flood
admission as a value-in/value-out rule. Its in-memory state currently mutates
map entries and reuses slice storage, including on refusal paths; separating
that mutation from admission needs its own plan.

<a id="current-checkpoint-r7-source-d47eef92-capture-pending"></a>

### Current checkpoint: R7 source d47eef92 (technical checkpoint complete)

This entry distinguishes the current implementation from the historical logs
above. The inspected revision is `d47eef92a1712bf40029475a2fcd12e8adfa0db4`,
tree `f69dc0f166a42e6dcd8035c3626e0a69bc4b8a1f`. Its corrected
[CI run](https://github.com/zalkilabs/zalki/actions/runs/43840322429) was observed
successful; that existing suite is not a quiet coverage measurement.
The independent static checklist found the boundaries acyclic with the named
exceptions above. The ownership supplement records the implemented moves.
The human report and quiet coverage were captured once at
`4e913c063d5cf014cc51b91e4fb81402f0657da1`, tree
`025a11e6b602e49873237f6e1ab8552f63b986a7`, in manual
[run 43863877249, attempt 1](https://github.com/zalkilabs/zalki/actions/runs/43863877249).
Independent raw review accepted these source-qualified measurements. The
independent static checklist and the technical summary for the owner are also
complete at their recorded source origins; this is not merge clearance.

Existing native evidence at source `9eb755dc40382aa51d586313ed8aeb73230b291d`
used Go 1.26.11 with native CGO enabled on Darwin and Linux. Its application
production inputs equal d47; d47 adds only the roundtrip ACK synchronization.
Those source-qualified snapshots report 458 exported declarations/shapes,
96 receiver identities, 51 internal import edges and 46 raw-OS importing files;
`Store` has 112 methods in 25 declaring files and `PluginFeed` 47 in 13.
These are selected-source observations with the AST tool's declaration/receiver
semantics, not old 99-method or 55/47-finding ceilings and not aggregate
complexity scores. Exact base/OS symbolic approvals live in
`scripts/architecture-delta/approved.json`; depguard remains canonical.
The following human measurements retain the separate 4e9 capture identity.

#### Captured measurements: 4e913c, Darwin/arm64

Go 1.26.11, CGO enabled. Human counts cover native-selected Go/cgo files under
`cmd/` and `internal/`, excluding other platforms, ignored files and `scripts/`.
Lines are physical; function spans include the declaration and closing brace.

| Package | Production files | Source lines | Test lines |
| --- | ---: | ---: | ---: |
| `internal/bus` | 61 | 8,277 | 26,244 |
| `cmd/zalki` | 53 | 8,080 | 13,581 |

`Store`: 112 methods / 25 declaring files. Six functions exceed 60 lines:
`cmd/zalki.runNotifyChild` 64, `pluginfeed.PluginInstall.Commit` 67,
`plugins.Run` 65, `plugins.launchChild` 61, `mcpserver.toolTable` 106,
`cursor.Listen` 62. Ownership improved; Store breadth, long functions and named
exceptions remain in the existing focused cleanup queue, not new count budgets.

The separate full native `-cover -count=1 -p=1 -json -timeout=10m ./...` run
(with a retained coverprofile) took 457.9048 seconds, exit 0, no command timeout:
25 packages, all 204 required roots passed. The `mode: set` profile contains
11,175 unique blocks / no duplicates: **12,628 / 15,208 statements covered
(83.03524%)**, weighting each positive-count block by its statement count.
This is a current native baseline, not a trend against a comparable prior
measurement or a percentage threshold.
The actual capture step used 480/1,200 seconds (720 remaining); its macOS job
used 825/1,800 (975 remaining). These are observed margins, not forecasts.
Nine nonrequired skips remain explicit: `cmd/zalki.TestTempAliasEnvironment`,
`adapters.TestClaudeSocketHelper`, `bus.TestEventUpdateLatency`,
`bus.TestSessionPathLatency`, `bus.TestSmokeClientHelper`, `bus.TestLiveClients`,
`journal.TestSyncMicroLatency`, `mcpserver.TestStdioRecoveryServerProcess`,
`plugins.TestInstallBuildProcessChild`. Helper/live/latency work is not credited.

The fresh CLI was not coverage-instrumented; this is not coverage of every child
or real client. CLI metadata/hashes were recorded, executables not uploaded or
independently rehashed. `agent`/`platform` have no direct selected test files and
0.0% own-package coverage; consumer tests can exercise them without default
per-package credit. Source836 fingerprint
`f2d76791d28a250d5b8a6e9f35d5fec9d6de2e08272f6da86ab881b53949ca0b`
was unchanged. Preserve source/VCS/binary/report/profile/run identity at 4e9
through reviewed docs/comments; never relabel these as later-head execution.
Artifact `14225820750` SHA256 is
`652b7d02b2a079a6511a7d5d9243685b9a45c264fa56023dc97c8c1931128db1`;
`architecture.md`: `bd6f19696437b5e2913a67a3fa2ee88b7f64d3b78a1af8ceef0d2eb56e379e0d`;
`coverage.out`: `b9574dd79f33089bf0ab4dcb7c25bfc9b19f8c744372eabc2bc384b7a398af04`.
The manual parent-only comparison does not replace the automatic PR's full
`fbf5582` policy comparison. The technical interpretation and summary for the
owner are recorded below, without creating a new owner-approval gate.

#### Summary for the owner

The model/journal/procwatch/plugin ownership boundaries and acyclic selected
import graph are clearer; documentation now matches the removed global fence
and named retained exceptions. Remaining work is explicit: `Store` has 112
methods in 25 files within 8,277 bus source lines, six functions exceed 60 lines,
and GoDoc, build-version, installer-helper and raw-OS exceptions remain in the
existing focused cleanup queue. These are observations, not new thresholds or
feature deferrals. Continue the existing consumer-interface, plugin-package and
complexity cleanup in separate reviewed slices. The native 83.03524% baseline
retains the skips and non-instrumented CLI/real-client limits above.

Measurements, independent static review and this technical summary complete the
checkpoint at their exact accepted inputs. Final current-head CI, premerge and
actual-main verification, and the original release/live/archive checkpoints
remain separate; this summary grants no new feature, release or merge authority.

The nine retained native `thelper` findings are assertion callbacks in
`internal/journal/codec_test.go`'s `TestReadJSON`; fixture helpers already mark
themselves. Keep case-specific assertion locations rather than silencing them
with `t.Helper()`. Existing agent-export GoDoc debt remains carried; the useful
new login-item API comments do not establish universal API-doc completion.
Characterization evidence must retain its actual source/profile: the R7 work
included fixture-field adaptations, native FD oracle corrections, the H2F2
secondary-assertion recipe correction and durable ACK synchronization. Original
profile/gap/security predicates remain; this is not a claim that every test file
was byte-for-byte unchanged across the migration.

#### Current behavior decisions and follow-ups

Historical characterization is evidence, not an endorsement or a new issue
resolution. The following statuses refer to current source/tests; no issue IDs,
merge commits or live-client validation are implied.

| Topic | Current disposition and evidence |
| --- | --- |
| Installer empty-hook lists, null/empty/whitespace input, partial preparation artifacts and map-order error priority | Carried behavior; changing it remains a separate open decision. See `internal/install/{remove_config,merge_config,apply_edits,validate_patch}_characterization_test.go`. Rollback of target contents does not promise removal of every backup/directory. |
| Retired-session retention after listener release | Carried/open: releasing a lease refreshes `UpdatedAt` for a still-retained session; expired sessions are deleted first. See `internal/bus/prune_gaps_test.go`. |
| Flood state ownership | Structural extraction implemented: `model.DecideFloodAdmission` clones map/slice inputs and `bus.consumeFloodQuota` retains load/decision/save under the journal lock. See `internal/model/flood_ownership_test.go`; the checkpoint-2 extraction paragraph is historical. |
| Flood exact-retry/conflict validation order | Carried/open behavior decision, separate from ownership: early return can precede later-arrival validation. See `internal/model/flood_order_test.go`. |
| Refused reply acknowledges its parent | Carried/open behavior decision. `internal/bus/reply_retry_quota_test.go` also guards retry quota; this is not a newly endorsed delivery guarantee. |
| Unreachable hook context and queued approvals | Carried/open: `EmitPending` can return empty context without error and settle queued approvals. See `internal/bus/emit_pending_gaps_test.go`; actor-binding refusals remain independently enforced. |
| Malformed-message scan behavior | The old abort-with-partial-views description is superseded: current scans skip/count unreadable records and keep later views in display order. See `internal/bus/read_path_privacy_test.go`, `message_unreadable_list_test.go` and exact-ID/cursor refusal cases. This does not turn every unreadable record into a successful exact read. |
| Rate-limit logging | Resolved in current source: `internal/bus/logging.go` preserves `CodeRateLimited`; `log_allowlist_golden_test.go` expects `error_code=rate_limited`. No R7 attribution or unverified fixing commit is claimed. |
| Observer shutdown and unknown evidence | Retained explicit contract: see `internal/bus/exit_observer_lifecycle_test.go`, `exit_observer_ownership_test.go` and [watch lifecycle](watch-lifecycle.md). Keep Close ownership and never infer exit from unknown. |
| Historical marker-fencing description | Removed by actual ancestor [396669e](https://github.com/zalkilabs/zalki/commit/396669e645860e9a941811e1dbe15b81d77a5602); the global marker/observer/MCP fence tests were removed with it. [Watch lifecycle](watch-lifecycle.md#retired-global-format-fence) and upgrading now describe current operation-specific record handling (`internal/bus/unsupported_record_test.go`), not a no-directory/no-notice guarantee. Archive/StageB/release authority and future migration policy remain separate open follow-ups; no current marker-fence pass or new runtime implementation is claimed. |
| Plugin process boundary | Retained limits: trusted local executables have no OS sandbox; child-only FD padding covers the complete captured bound, assumes no new inheritable FD above it, and inherits Go's unchecked post-fork close statuses. Inventory/cap/setup errors refuse launch. Native all-FD tests remain the actual inheritance oracle; parent handles/flags stay unchanged. See `internal/plugins/child.go` and `cmd/zalki/plugin_supervised_fd_test.go`. |
| Notification and live platform behavior | No automatic notification recovery or new managed-job/wake authority is implied. Live login-item/launchd and real-client validation remain separate pending work; hosted synthetic fixtures do not close it. |

The technical checkpoint measurements, independent review and summary for the
owner are complete at their source-qualified origins. Current-head CI, premerge,
actual-main and original release/live/archive gates remain separate. No behavior
is changed or deferred by this documentation layer.
