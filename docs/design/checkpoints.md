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
| Unused-by-production lock primitives | deferred from `internal/journal` | Step 3b (introduced with their caller) |
| Installer-helper exception for clients | `install.HookConfig`, `HookCommand`, `MCPConfig`, `ShellQuote` | A helper-extraction decision before step 6b |

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
