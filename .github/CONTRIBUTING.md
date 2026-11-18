# Contributing

Use Conventional Commits subjects for commits and pull request titles: type: summary (or type(scope): summary), choosing feat, fix, refactor, test, ci, docs or chore for the actual change. Squash merges retain the reviewed pull request title.

Use Go 1.26.10 or newer. Work on a short-lived branch and open a focused pull
request describing the problem, resulting behavior, and validation. Squash
merge after CI passes. Maintainers are responsible for reviewing and testing
AI-assisted changes as they would any other contribution.

```sh
make fmt-check test vet lint
```

CI runs `make test-required`: the uncached race suite plus a JSON coverage gate
requiring real adapter transports, Cursor listener/recovery, and the emulated
bidirectional roundtrip. Missing tests or skipped required tests/subtests fail
the gate on both Linux and macOS. Ordinary `make test` permits local socket
sandbox skips. Live model tests stay opt-in; subprocess helper skips are valid.
CI also runs `make test-alias` on Linux and macOS. It proves that the test child
uses an owned symlinked temporary root, runs the full race suite, and rejects
missing or skipped required tests with the alias coverage checker. The ordinary
and alias passes keep separate reports. These checks use the versioned required
test list; adding a required test updates both modes without a workflow count.
The existing `Test` checks wait for alias coverage and the Windows reporter
controls. They explicitly fail if either dependency fails, is cancelled or is
skipped; adding these jobs does not require a branch-protection settings change.

`make windows-vet-test` runs fake-Go discovery controls before the Windows report.
Partial, diagnostic-bearing or empty discovery fails setup before vet; per-package
Windows vet findings remain report-only. CI retains discovery lists, diagnostics,
and individual vet logs along with its summary.

The existing `Lint` checks also compare strict diagnostics against the actual base
commit on each OS. Both trees run normal-then-strict and strict-then-normal with
separate initially empty linter caches. Exact repository-relative file,
function/declaration, linter, literal message and source-text keys are compared as
multisets. Extra copies of an existing diagnostic also fail. Physical line-number
movement is not a new diagnostic; replacing source text under the same other keys
requires review. Order-dependent reports and malformed input fail the check.
Source links are unsupported: snapshots reject them, and scoping reads are confined
to the opened source root before any AST or source-text evidence is accepted.
The stricter findings remain report-only in `make lint-strict`; CI enforces the
base/head ratchet without a fixed diagnostic count. Both orders also require
normal lint to pass. Explicit base/head commits, tree/config/tool hashes, logs
and raw/scoped reports are retained. Tag/manual runs use the candidate's first
parent if no event base is available; pull requests and pushes must supply an
actual base. No local checkpoint is replaced until hosted equivalence is reviewed.

Production core, installer, adapter composition and client dependencies are
restricted by depguard in `.golangci.yml`; external composition tests are exempt.

Use `make fmt` to format code. Add tests for behavior changes and regressions.
Fixtures must use temporary state and configuration directories. Normal tests
must not contact live agent models or modify your real client settings.

For dependency checks:

```sh
go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
make vulncheck
```

Report security issues using [the security policy](SECURITY.md). Remove private
capabilities, message bodies, credentials, and personal paths from reports.

## Client integrations

Codex, Claude and Cursor transports, hook mappings, name lookup, configuration
and native guidance live in `internal/adapters/codex`, `claude` and `cursor`.
Each client exposes a concrete `Adapter` and `New` constructor implementing
`bus.Adapter` and its lifecycle contracts. `internal/adapters` only composes
these constructors in `Builtin`; the CLI injects this registry into core.
Clients may depend on bus/install, but never the parent composition package or
each other. The small `internal/adapters/internal/socketio` leaf shares only
Claude/Cursor context cancellation and deadline binding, with joined cleanup;
it depends only on the standard library. Receipt and configuration-root helpers
remain private to each client.

Protocol/parser tests stay with their client. Full-registry discovery, lifecycle,
listener/recovery and fourth-adapter tests use the external `adapters_test`
package in the parent directory. `TestExternalAdapterComposesWithBuiltins`
exercises the extension path from outside core. Keep the exact owning package
paths in `scripts/check-integration` up to date when moving required tests.

Cursor's native Shell guidance, hook policy, metadata and sending protocol live
with its adapter. The authenticated `ListenCursor` runtime remains in
`internal/bus`: it owns the lifetime lock, receiver framing and pending-message
validation, generation/endpoint-bound registration and cleanup, and notification
output. Its persisted listener state/migration and atomic prompt, suppression and
recovery claims also remain core. The adapter requests trusted hook intents and
formats a recovery claim; it cannot read or mutate the journal or rotate a
capability itself. Keep that boundary intact when changing the integration.

Keep identity, authentication, retry, and receipt rules in the shared core.
Client protocol changes usually affect its adapter and fixtures; changes to hook
input or configuration formats can also require shared-helper updates. Verify
both send and receive behavior against the affected client's native protocol.

## Compatibility and cost checks

Client contract fixtures live in `internal/bus/testdata/clients`. Existing fixtures
are normalized observations from earlier disposable-session probes, not raw
captures or a guarantee that current client versions work. `zalki doctor` shows
the installed versions, fixture coverage, listener health, and recovery hints.
Already-running clients may still use an older version until restarted.
Claude's adapter additionally owns an exact-version/platform evidence table.
`live_verified` describes historical smoke coverage; it does not turn a
`transport_written` receipt into native acceptance or a read receipt. See the
[Claude socket investigation and smoke recipe](../docs/design/claude-delivery-ack.md)
before adding verification for another release.

To capture a new startup contract, export `ZALKI_CAPTURE_CONTRACTS` as an absolute
private directory before starting the client. Hooks capture allowlisted schema
fields with normalized IDs and paths; prompts, titles, credentials, and tool
inputs are omitted. Review the resulting JSON before adding it to the fixtures.

Normal CI uses fixtures and never invokes live models. For a manual live check,
first load the current Zalki build in fresh disposable client sessions sharing
one workspace and state directory, then run:

```sh
export ZALKI_SMOKE_STATE=/path/to/private/state
export ZALKI_SMOKE_WORKSPACE=/path/to/disposable/workspace
export ZALKI_SMOKE_TARGETS='codex:EXACT_UUID claude:EXACT_UUID cursor:EXACT_UUID'
make smoke-live
```

The runner sends real review requests and records correlated replies, receipts,
versions, and latency in private state reports. It creates and retires its own
receiver identity; it never changes client configuration or permissions.
Fresh-start, idle, and draft checks require operator confirmations. Restart
checks also require a changed registration generation or process-owner fingerprint. Nothing is automatically
resent. To run only the automated round trip, set `ZALKI_SMOKE_CASES=roundtrip`.
Native model usage applies; token counts and internal model turns are reported
as unknown because clients do not expose a common usage interface.

```sh
make cost-check
```

This benchmarks inbox scans versus exact-ID reads at 100, 1,000, and 10,000
records, then measures request/response and tool-schema bytes for a scripted
review/revision exchange and an eleven-agent discovery roster. The budgets are
6 KiB of responses for the exchange, 10,910 bytes for all tool schemas (including
`watch_agent`, `set_task`, authenticated self marking, and sent preview/filter
options), and 150 bytes per agent in the eleven-agent discovery fixture
(including the envelope).
Discovery keeps per-session `delivery`, optional `experimental`, and `updated_at`
in UTC seconds. The ID prefix gives the kind; omitted state/liveness mean
ready/unknown. Idle wake is implied except for `next_hook`; `idle_wake` overrides
unusual adapter combinations. Full transport and setup capabilities stay
in the unchanged CLI `list` and `doctor` views, also used by live smoke checks.
It calls no live models; bytes are a portable cost
proxy, not a token or billing estimate.

The SDK mirrors structured results into text for
[MCP compatibility](https://modelcontextprotocol.io/specification/2025-06-18/server/tools#structured-content).
The discovery cost log includes text and complete tool-result bytes as well as
the structured payload; these are distinct measures. Do not assume that every
client inserts both channels into model context, or that every client can read
a structured-only result.

`list_messages` returns `data.messages`, `data.more`, and `data.next_cursor`;
continue pages with the same filters and `cursor=next_cursor`. It defaults to five unread
records, and bounds normal pages to 24 KiB. Wake handlers should request the
hint's `message_id` directly, avoiding a journal scan. History is opt-in and omits
bodies unless `include_body=true`; exact reads return complete bodies.
Sent history also supports `to` (exact ID or current unique name) and opt-in
`preview:true` (one line, at most 80 UTF-8 bytes). These options require
`sent:true`. Previews never bypass deferred native inbox delivery. Alias fields
are rewritten in receiving middleware before SDK validation, without adding
alias properties to the canonical schemas.
`body_omitted` means fetch by ID; `body_deferred` means await native delivery.

## Shared state upgrades

List and discovery skip individual unreadable, unsupported, or removed-adapter
session records. CLI list reports a count on stderr; MCP list_agents adds
`skipped_sessions` only when nonzero. Doctor and prune retain diagnostic reasons.
Exact reads and writes still reject those records. Their bytes are kept intact.

Session, message, watch, and flood rewrites preserve unknown JSON fields, including
retained nested records. Known fields explicitly cleared are removed, including
case variants. Adapter migrations explicitly identify obsolete extension fields;
old wake endpoints are removed instead of being revived by preservation.

`state-formats.json` is a versioned, lazy declaration of formats written by
marker-aware builds. It records each family's highest schema before publishing
the record and never automatically lowers that declaration. It is not a census
of legacy files: unmarked records still rely on per-record schema checks. Readers
do not create a marker unless they actually recover a pending reply transaction.

A newer, unknown, or unreadable marker fences journal mutations with an
`upgrade zalki` error. Known sessions and message pages remain readable; reply
recovery waits for a compatible build. MCP can start without runtime registration,
and lifecycle hooks become logged no-ops, so the fence does not break the client.
Diagnostics may continue writing logs and using the existing journal lock; the
fence protects record formats, not operating-system access to the directory.

Pre-marker binaries ignore the file. They do not gain forward protection from
its presence: upgrade every writer sharing a state directory. The schema-2 lease
fence remains necessary. A marker left after a failed record write conservatively
keeps the announced format requirement rather than risking an undeclared record.

## Releases

Maintainers release reviewed commits from `main` using tags such as `v0.1.0`
or `v0.1.0-rc.1`. Pushing a tag runs CI, builds and checks macOS archives and a
source archive, generates an architecture-specific binary Homebrew cask, and
creates a draft release with release notes. Verify the draft, then publish RC
tags as prereleases without marking them Latest. Public prereleases support
anonymous downloads; drafts and private repositories require GitHub access.
Tagged releases must be signed and
accepted by Apple notarization before packaging. Only the draft-creation job has
repository write access. Local and CI snapshots do not use signing credentials.

Configure these Actions secrets before creating a release tag. Organization
secrets must grant access to this repository:

| Secret | Value |
| --- | --- |
| `MACOS_SIGN_P12` | Base64-encoded Developer ID Application identity, including its private key |
| `MACOS_SIGN_PASSWORD` | Password protecting that identity export |
| `MACOS_NOTARY_KEY` | Base64-encoded App Store Connect Team API private key |
| `MACOS_NOTARY_KEY_ID` | API key ID |
| `MACOS_NOTARY_ISSUER_ID` | API issuer UUID |

Export only the release signing identity from Keychain Access as a
password-protected `.p12`. Enter credentials directly from your own terminal;
do not put private key contents in chat, commits, or command arguments:

```sh
base64 -i /path/to/release-signing.p12 | gh secret set MACOS_SIGN_P12 --repo zalkilabs/zalki
gh secret set MACOS_SIGN_PASSWORD --repo zalkilabs/zalki
base64 -i /path/to/AuthKey_KEYID.p8 | gh secret set MACOS_NOTARY_KEY --repo zalkilabs/zalki
gh secret set MACOS_NOTARY_KEY_ID --repo zalkilabs/zalki
gh secret set MACOS_NOTARY_ISSUER_ID --repo zalkilabs/zalki
```

The commands without piped input prompt for their values. A release fails if
any required secret is missing. The tap's versioned cask installs the signed
binary and bundled completions; manual development builds require Go.

Before publishing the draft, check the assets and release notes and test native
Claude, Codex, and Cursor messaging, idle wake, draft preservation, and
install/upgrade/uninstall on a disposable macOS setup. Record the tested client
versions in the release notes. Automated fixtures do not establish live-client
compatibility. Keep prebuilt macOS signing/notarization limits clear to users.

Local packaging checks require GoReleaser v2.18.3:

```sh
make release-snapshot
```

Source archives include committed files only. `dist/zalki.rb` is the generated
release cask; review it before updating the tap. An existing release is never
overwritten automatically. Delete an incomplete draft before retrying its job.

After the first push, configure GitHub to require the CI checks and pull requests
on `main`, block force pushes/deletion, and protect release tags. Solo maintainers
can merge their own passing PRs. Repository protection and private vulnerability
reporting are GitHub settings, not enabled by adding these files.

### Exact API and architecture comparison

The native Linux and macOS Lint jobs also run the versioned `arch-metrics -json`
collector against the same immutable base and head worktrees used for strict
lint. The normal human-readable `make arch-metrics` report remains available.
Both snapshots retain the selected Go build identity, the repository's named
export ledger, declaration shapes, every receiver's method/file counts, internal
import edges, and raw OS-import users across all production OS files. Fields and
interface members contribute to type shapes, not additional named-export counts.
Exported methods on private receivers are counted, as in the existing report.
Scripts are tooling rather than application API. The raw OS exception list must
match measured users exactly; wildcard growth is refused.

These are source declaration shapes, including wire tags, type aliases, function
signatures, pointer/value receivers, and full constant-group initializer context.
They are not a universal ABI or constant-evaluation guarantee. Native build
selection is measured on both operating systems; it is not inferred from one OS
or from old numeric totals.

An unreviewed added, removed, or changed identity fails the comparison, including
a change that preserves a numeric total. An intentional change requires an exact
entry in `scripts/architecture-delta/approved.json`, containing the actual base
commit, native OS, complete `delta` object from that failed run's result, and a
review reason. Review the measured changes before adding an entry. A stale base,
other OS, partial delta, numeric ceiling, or duplicate matching allowance cannot
supply review credit. Historical entries are not replayed on a new base.

Run `make architecture-delta-test` for pure orchestration/policy fixtures. The
native collector roots are required in ordinary and alias coverage. Fake tools
prove failure handling; actual compiler, snapshot, keyed lint, timeout margins,
exact-head hosted coverage and final-main review remain required before retiring
any equivalent local gate. Live client installation and smoke checks are separate.

### Focused compiled mutation controls

The native focused-control jobs retain the reviewed production and runtime
configuration mutations in `scripts/fault-controls/controls.json`. Application Go,
module, build, release configuration, workflow or tooling changes run the complete
current focused plan. Other changes produce an explicit zero-execution result;
the job still runs and the protected Test dependency requires its success. Base
and head are actual event commits. Control IDs already present in the base plan
cannot silently disappear from the head plan.

Each selection runs in a disposable head worktree with synthetic native-client
homes, a private temporary directory, offline module use after explicit warming,
and bounded commands. Good, mutated and restored source are compiled separately.
Every phase retains source identity, build and test receipts, binary hashes and
raw test2json. Restoration checks all source bytes and modes. Compile/setup errors,
timeouts, races, panics, skips, incomplete streams, foreign packages and sibling
or prefix-lookalike tests cannot count as an intended assertion failure.

The versioned `assertion-sites` tool parses the current Go test source and binds
the complete quoted fatal call, exact site count, helper/caller graph and report
positions. It checks production mutation preimages inside their named function;
the release-linker configuration control remains a runtime-file mutation. No
historical line number is an assertion selector. A new testing callback has its
own testing context: a helper-marked parent does not make its child's fatal line
report as the parent's caller. Both the callback context and the root-to-helper
connection are verified for that control. The two reviewed RPC branches are one
explicit site set; an extra site is refused.

The stream checker requires an exact selected RUN/outcome, consistent package
outcome, and current source-bound intended assertions for failing selected leaves.
It reports assertion selections, unique source mutants and unique compiled mutant
hashes separately. Test binaries stay on the isolated runner; artifacts retain
hashes, logs and source/receipt records rather than uploading every binary.
Synthetic parser fixtures and fake tools are not evidence of actual compiled
controls. Native unit tests, the real focused plan on both operating systems,
keyed strict comparisons, exact-head coverage, timeout margins and final-main
review remain necessary before equivalent local controls can be retired.

Native CI also runs full, uncached **unraced** tests of directly touched
native package directories, with a fresh unraced CLI supplied to their integration
tests. Module/dependency changes select all current native packages; runtime file
changes select packages declared by their control recipes. The runner derives
package identities from Go discovery, explicitly records deleted or native-excluded
directories, and rejects unknown/partial discovery. It uses no test-name filter and
projects the current compiled `check-integration --list-required` ledger onto the
selected packages. Missing required RUN/PASS, required descendant skips and missing
package outcomes fail. Optional skips and verified no-test-files packages remain
explicit in the result, as in ordinary required coverage.

The fresh CLI is built with race instrumentation disabled and VCS stamping required.
Its embedded JSON build information must match the clean actual head, native SDK
identity and non-race mode; its hash must remain unchanged through the tests. This
path replaces neither the raced ordinary suite nor the symlink-temp alias suite.
Actual source-head package selection, raw reports and measured timeout margins are
required before a corresponding local unraced package run is replaced. Real-host
installation, migration and client smoke remain separate.

Native fixture changes also select their owning package: for example a change
only to `cmd/zalki/testdata/fdprobe.c` runs full unraced `cmd/zalki` tests. Packages
own their input subtrees, with nested `testdata` Go programs kept as fixtures.
`input-owners.json` records additional consumers for shared fixtures and explicit
runtime inputs. Deleted fixtures keep their current owner; whole-package removal
is reported explicitly. Unknown runtime inputs and stale mapped owners refuse
coverage rather than returning an unqualified zero-package success. Reviewed
non-runtime documentation/CI classifications are retained in the input bindings.

### Large Codex PostToolUse envelopes

Codex PostToolUse accepts one complete JSON object up to 32 MiB on the wire.
A streaming selector validates and discards only the top-level `tool_input` and
`tool_response` values; the remaining object stays within 8 MiB and uses the
ordinary typed decoder and lifecycle identity/generation checks. Duplicate
or case-aliased top-level keys and malformed ignored values refuse the event
before state changes. Nested lookalikes cannot select identity or the allowance.
Other events retain their 8 MiB admission limit; other agents use the existing
decoder. A late Codex event name may require scanning up to the finite 32 MiB
limit before that decision. Extended reads check caller cancellation between
reader fills; this cannot interrupt an arbitrary reader already blocked in Read.
The selector returns after the first complete object without waiting for EOF.
The 32 MiB ceiling covers the demonstrated 9 MiB tool result and similarly sized
input plus result together; it is a bounded compatibility allowance, not an
upstream maximum or a promise to accept every possible native envelope.
