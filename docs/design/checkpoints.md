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

| Bridge | Where | Removed by |
| --- | --- | --- |
| `type processInfo = procwatch.Info` | `internal/bus/process.go` | Step 5 (model moves), when bus uses `procwatch.Info` directly |
| Done-aware forwarding source wrapper | `internal/bus/exit_source.go` | Step 5, with domain process values |
| Installer-helper exception for clients | `install.HookConfig`, `HookCommand`, `MCPConfig`, `ShellQuote` | A helper-extraction decision before step 6b |

Any compatibility aliases and forwarders introduced by step 5 must be added
here when they exist, with their removal step.

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
