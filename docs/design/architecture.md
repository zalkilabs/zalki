# Target architecture

Status: reviewed design proposal, revision 2. This records the migration and
extension plan; it changes no runtime behavior. Future features remain
subject to separate implementation review and explicit user enablement.

## Where we are

The macro structure is sound: the import graph is acyclic, `depguard` enforces
the boundaries, adapters plug into the core through small optional interfaces,
and `internal/adapters` is the only composition root. The weakness is inside
`internal/bus` (49 production files). Measured by Go AST: 99 `Store` methods
across 26 files, 193 exported top-level declarations plus 302 exported struct
fields and interface members.

- `Store` mixes record storage, domain rules (status transitions, approval,
  flood admission, prune decisions) and orchestration.
- 13 files touch the filesystem directly.
- Other packages use about 99 distinct `bus.X` names, but not every exported
  name is dead weight: some are JSON protocol fields and interface methods.
- OS process mechanics (kqueue, pidfd) live beside domain code.
- Extension is compile-time only, and one agent cannot start another.

The problem is not the file layout (fixed in #22 to #25). It is that the
storage transaction, domain rules and adapter contracts are entangled by
shared values, private methods and one lock.

## Principles

1. Dependencies point inward: data and consumer-owned interfaces at the center,
   mechanisms at the edge.
2. A package has one reason to change and a small exported surface.
3. Domain rules are pure functions over values; IO is load, decide, save.
4. Every cross-process boundary (client hooks, plugins, spawned agents) is a
   versioned, bounded, validated protocol with explicit trust.
5. Each step is behavior-preserving and gated by characterization tests written
   before the move (see Migration). A step that needs a bridge states it.
   Existing tests count: add tests only for an identified boundary gap, rather
   than duplicating coverage or mirroring implementation details.
6. Honest claims: out-of-process is crash isolation, not an OS sandbox; a
   headless wake is a new execution authority, not a delivery detail.

## Target packages (conditional on the prerequisites below)

```
cmd/zalki             CLI composition root
internal/mcpserver    MCP driving adapter                        -> bus
internal/bus          application layer and the storage
                      transaction: Service, Store, recovery      -> model, agent, journal, procwatch
internal/agent        adapter SPI and registry                   -> model
internal/model        data types with their behavior and pure
                      rules                                      -> stdlib
internal/journal      bounded record codec, atomic replace,
                      directory sync, lock primitives            -> fsutil
internal/procwatch    raw OS process identity and probes,
                      exit sources (kqueue, pidfd)               -> stdlib, x/sys
internal/adapters/*   built-in clients                           -> agent, model (+ listener/endpoint ports; install helper exception)
internal/plugin       out-of-process native adapters             -> agent, model
event subscription    CLI/application port owned by bus; consumer uses protocol
internal/install, fsutil, logging   unchanged
```

Prerequisites that the first draft skipped:

- **Value ownership.** `Session` embeds `ProcessRef`, `ListenerLease` and
  `ReceiveCapabilities`; the last refers to enums in `adapter.go`; `ownerFrom`
  consumes `AdapterDescriptor`; `processLiveness` depends on `Liveness`. These
  small values must be assigned an owner first. `procwatch` takes only raw OS
  identity and probes. Adapter-dependent owner selection and session liveness
  policy stay in `bus`.
- **Behavior moves with the type.** A type alias cannot attach methods to a
  foreign receiver or grant access to unexported members. `Message.move`,
  `hold`, `settleNotSent`, `releaseHeld`, `applyReceipt`,
  `ReceiveCapabilities.validate` and `Receipt.cause` are package-private. Each
  type moves together with the behavior it needs, or an explicit narrow
  exported bridge is designed and reviewed first. Aliases are only a
  compile-time convenience for exported identifiers.
- **The transaction stays in `bus`.** `Store.locked` checks write
  compatibility, rejects unsupported writes, recovers reply intents and then
  runs domain work under one flock (`store.go`, `message_reply.go`).
  `journal` supplies bounded codecs, atomic replacement, directory sync and
  lock primitives *beneath* a bus-controlled transaction, with explicit error
  translation. Recovery is never moved before or after the lock to satisfy
  imports. Any lifetime-lock acquisition while the journal lock is held must
  be non-blocking; a `Lease` may take the journal lock while holding its
  lifetime lock. Preserve that deadlock-avoidance invariant rather than assuming
  one universal acquisition order (`listener.go`). State-format family lists
  and domain error classification stay with the domain.
- **Adapters are not "agent plus model only" yet.** Cursor's `Listen` takes a
  concrete `bus.Store` and a `Lease` with private store, key, fd and endpoint
  state. `ConfigurationAdapter` exposes `install.ConfigFile`. Before moving the
  SPI, define a consumer-owned listener port and decide where the configuration
  DTOs live; prove them on Cursor first. Native endpoint inspection/checks,
  identity/envelope helpers, diagnostics and the Codex build-version handshake
  are additional bus edges that need value ownership or composition injection.
  Clients also call installer configuration builders and quoting helpers;
  retain that existing leaf dependency explicitly in the first refactor, or
  review another extraction before claiming the client import list is closed.

## Migration

Phase 1 is a sequence of small PRs, each preceded by tests that pin current
behavior at the boundary being moved. Six mechanical PRs understated the work.

0. **Characterization first.** Add or confirm tests for: persisted schema and
   error codes, reply crash recovery (intent present or absent at each step),
   listener lease and lock ordering, delivery receipt uncertainty
   (`not_sent` versus `unknown_*`), and prune. These run unchanged across every
   later step.
1. **Value ownership map.** Document who owns `ProcessRef`, `Liveness`,
   `ReceiveCapabilities`, `ListenerLease`, `Endpoint` and the adapter enums,
   and the dependency bridge each needs.
2. **procwatch.** Move only raw OS mechanisms (exit sources, process reading
   and identity) behind that map. Owner selection and liveness policy stay.
3. **journal.** Extract the generic bounded record codec and atomic
   operations. `bus` keeps transaction and recovery orchestration.
4. **Listener port and configuration DTOs.** Prove them on Cursor.
5. **model.** Move each type with its behavior; add explicit bridges for any
   private state still needed. `Receipt.WithCause` already exists: replace its
   private-field literal with that setter, add a nonserializing cause accessor
   for current private reads, and preserve both when moving the type. Pure
   rules (status machine, flood admission, approval resolution,
   prune decisions) follow as functions over values with file-free table tests.
6. **agent SPI.** Only after 4 and 5. Add the `depguard` rules in the same PR.
7. **Trim.** Unexport names that nothing needs, excluding JSON protocol fields
   and interface members.

## Plugin system

Goal: let third-party authors add a client or coordination feature without
rebuilding zalki. Third-party authoring is in this proposal's target scope;
installing and enabling any particular plugin still requires explicit user
authorization. Plugins are local executables speaking a versioned protocol;
native Go shared-library loading is outside this design.

**Trust, stated plainly.** An out-of-process plugin gives crash and protocol
isolation, not an OS security boundary. A same-user executable can read the
user's auth and configuration files and run other commands regardless of its
manifest. Third-party programs are **untrusted by default**; enabling one
explicitly trusts it to run with its OS privileges. Installation does not
enable it. Enablement (`zalki plugin enable <name>`) shows a clear grant prompt
naming data fields, destinations, filesystem roots, executables and send scope,
with revocation. These grants constrain core APIs, not arbitrary same-user
code. Plugins are never auto-enabled from a repository or received message.
Core-mediated operations are limited to what the manifest declares; OS-level limits exist only if we add OS
isolation, which is out of scope for the first version. We pin the manifest
and the executed artifact by digest, resolve paths without symlink or TOCTOU
substitution, use argv (never shell interpolation) and bound stderr, output
and runtime. `doctor` lists enabled plugins, pinned versions and effective
grants without printing credentials or private payloads. Crashes never block
core; rate and byte limits protect subscriptions, and a slow or abusive
subscriber can be disconnected without blocking journal writes.

**Public authoring contract.** Publish protocol and event-schema versions with
a semantic-versioning, compatibility and deprecation policy; mark RC-stage
surfaces experimental and describe their stability limits explicitly. Provide
fixtures, a conformance test kit authors can run, and a minimal documented
reference subscriber example. This example demonstrates the contract, not a
bundled workflow feature. Define major-version negotiation and reject
incompatible versions rather than guessing. Distribution and discovery are
out of scope initially: a manifest and local installation suffice, with no
marketplace. Leave room for future signing and registry provenance without
claiming digest pinning proves a publisher's identity.

**The Go SPI is not mirrored literally over RPC.** `LifecycleHost` and
`Lease.Confirm` pass Go closures that run under the journal lock; an RPC call
there would hold the whole journal hostage and cannot serialize closures.
Plugin interfaces use bounded declarative inputs and results; core re-reads and
re-validates generation and state itself. Host actions are bound to the
authenticated session and return sanitized DTOs, never the persisted `Session`
(which contains `KeyHash`).

**Delivery outcomes.** A plugin crash or timeout cannot simply become
`not_sent`: once native submission may have happened the outcome is unknown,
and retry risks duplicates. The protocol defines call IDs, the acceptance
boundary, cancellation, duplicate suppression and receipt validation before
any plugin delivers real prompts. `transport_written` stays distinct from
agent acknowledgement and task completion.

**Declarative adapters are a limited subset.** The built-in clients are not
declarative today: Cursor remaps and normalizes hooks and claims recovery;
Codex preserves generation only for compaction and filters native prompt
completion; Claude delivers over a framed socket with size bounds. The first
manifest format supports only a deliberately small subset (name mapping,
executable, process names, capabilities, a delivery command with argv
template) and lists explicit "custom adapter required" cases rather than
growing a general policy language. Process plugins are specified as either
long-lived per server or per call, with that choice made per hook class
(hooks run once per client event and need a fast path).

## What the ecosystem builds

The core does local, cross-vendor, serverless messaging with delivery receipts
and verified identity, and nothing else. Research, November 2026 (secondary
sources; verify each project before depending on it):

| Category | Examples | Fits as |
| --- | --- | --- |
| Peer messaging and mailboxes | [claude-peers-mcp](https://dev.to/alanwest/how-to-coordinate-multiple-claude-code-agents-without-losing-your-mind-1i9f), [agent-link-mcp](https://dev.to/alanwest/how-to-make-claude-codex-and-gemini-collaborate-on-your-codebase-40l2), [AgentsRoom](https://agentsroom.dev/features/agent-messaging), [MCP Agent Mail](https://github.com/Dicklesworthstone/mcp_agent_mail) | Core (this is zalki). Crowded; the edge is cross-vendor, local, serverless, receipts |
| Native team features | Vendor-native messaging and task tools; capabilities vary by version and must be checked against primary documentation | Bridge plugin |
| Task boards and handoff | Agent Teams tasks, [Beads and Gas Town](https://github.com/steveyegge/beads) | Plugin: event subscriber plus its own tools |
| File reservations | MCP Agent Mail advisory leases plus a pre-commit guard | Plugin: tools plus a git hook |
| Orchestrators and spawners | [claude-team](https://pypi.org/project/claude-team-mcp/), [Open Orchestrator](https://dev.to/gitpcl/i-got-tired-of-switching-between-ai-coding-sessions-so-i-built-open-orchestrator-4p4n), Orchestra | Managed-job provider plugins |
| Observability | [disler hooks dashboard](https://docsearch.algolia.com/mcp/docs/repo/disler/claude-code-hooks-multi-agent-observability), agents-observe | Event-subscriber plugins |
| Interop standards | A2A and other agent protocols; native support and bridge requirements need version-specific verification | Bridge plugin |
| Human in the loop | MCP Agent Mail overseer UI, notifications | Notification sinks, human-origin messages |

### Extension points, in order of value and risk

1. **One scoped metadata event subscription. This is the whole first
   milestone** (not events plus verify plus send plus adapters plus spawn).
   Grounded in our own Claude/Codex review-and-publish workflow, it supports
   four plugin designs without building the plugins: a review and handoff
   board (exact tested SHA, PR URL, owner, review state, keeping "reviewed",
   "pushed", "merged" and user approval distinct); ready-for-review and
   blocked-work notifications to user-chosen destinations; a metadata
   timeline and export for incident reconstruction; and delivery and lifecycle
   metrics (hold duration, queue latency, receipt transitions, failures; never
   invented token or cost figures). Later, separately: advisory file
   reservations, worktree helpers and checklist tools, which are workflow
   policy, not messaging. The feed supplies messaging facts only: a board
   keeps explicitly supplied SHA, PR, review and approval records in its own
   store or obtains them through separately authorized integrations. It cannot
   infer a tested SHA, a merge or human approval from delivery receipts.
2. **Scoped send**, a later independent grant (below).
3. **Adapters** (new clients) and 4. **managed-job providers**, below.

**Event contract.** Versioned events carry: event ID, a durable cursor, actor
and session-generation IDs, event type, the real receipt status, a timestamp,
correlation and causation IDs, and the subscription scope. Plugins keep their
own materialized state and checkpoints. The contract specifies at-least-once
replay with deduplication, bounded retention and backpressure, an explicit gap
indication, and a scoped reconciliation snapshot. A **durable outbox must
commit in the same recoverable transaction as the state mutation**; appending
events afterwards can silently lose them on a crash. A flock serializes
writers but does not make multiple file replacements crash-atomic. Before
implementation, define a durable intent/commit point, idempotent recovery,
event ordering and cursor allocation across concurrent processes, and inject
crashes at each write boundary. Specify recovery and safe failure when disk is
full, retention reaches its limit or an event cannot be persisted; never drop
a required event silently. New records and recovery writers must participate
in state-format fencing, with mixed-build compatibility tested.

The existing `logging.go` is privacy-conscious, best-effort and has no replay IDs or cursors, so it is not
an audit API. If the first stream is best effort, its limits are documented and
it does not promise reliable audit. A best-effort stream does not satisfy the
durable first milestone. Subscription authorization applies to replay and
snapshots too, including grant revocation; a cursor is not authority to read.
Snapshot and cursor must form a consistent handoff so reconciliation cannot
silently miss events. No plugin I/O or blocked consumer runs under the journal
lock. The CLI uses a bus-owned subscription port; native adapter SPI types do
not own this application-level stream.

**Default events are minimal.** Beyond omitting bodies, they omit
capabilities and key hashes, display names, working directories, task text,
response snippets, raw errors and native output; names and paths are personal
metadata. Any richer field is an explicit grant. Body access, if ever added, is
a separate scoped operation that respects omitted and deferred delivery. The
plugin API grants no direct journal access (the trusted-local-program limit
still applies), and basic subscribers need no credential belonging to another
session. Core keeps authentication and grant
enforcement, canonical recipient resolution, generation and retirement checks,
idempotency, flood and approval admission, receipt transitions and the
transaction commit; plugins cannot override them through callbacks. Schemas
and byte bounds are validated before publishing.

**Operational traps the protocol and docs must preserve:**

1. Route and store canonical IDs plus generation, never display names, which
   are mutable and can be ambiguous.
2. The 16 KiB limit is UTF-8 bytes, not characters; NUL and leaking the
   sender's own capability are rejected; native JSON framing can add another
   size limit.
3. `transport_written` is not a model acknowledgement or task completion;
   unknown submission is unsafe to resend. Retries reuse the exact message ID;
   never infer missing delivery from delayed receipts or poll deferred bodies.
4. Flood limits and approval holds (queue capacity eight) are real outcomes,
   not failed sends. Plugin reactions must be idempotent, use their own
   causation IDs and bounded work, so replay or notifications cannot create
   loops. Ignoring self-caused events helps but is not a semantic loop guard.
5. `updated_at` is not a heartbeat and native liveness can be unknown. A board
   must display uncertainty and never retire a session itself.

**Needs a permission model before enablement:** reading or exporting bodies or
transcripts; sending externally; injecting, routing or redacting prompts;
acting as a user; registering executable tools; filesystem, git or worktree
mutation; scheduling; spawning or terminating processes. Grants name data
fields, destinations, filesystem roots, executable and actions, and send scope,
with revocation. An agent's "approved", a model acknowledgement or a
review-board label never becomes human approval. A same-user plugin remains a
trusted local executable unless OS isolation exists; API grants alone do not
sandbox it. Every grant beyond scoped metadata events requires a security
review gate before the feature is enabled, including bodies, scoped send,
tools and managed jobs. The initial event scope and its authorization still
require review; “metadata” is not synonymous with public data.

**Scoped send** acts as the plugin's own principal with recipient, method and
rate limits and revocation. It never impersonates another agent and cannot
manufacture quota-exempt core notices. Extra tools (task boards, reservations)
are ordinary MCP servers; if zalki later proxies tool calls it uses namespaced
tools with validated schemas and core-authenticated scoped subjects, avoids
name shadowing, and never forwards bearer session capabilities.

**Identity verification is not in the first milestone.** The earlier `verify`
idea does not provide the claimed property: if a third-party tool receives
actor plus capability to call verify, it has received the bearer credential;
if the agent calls verify and passes only the returned kind and ID, the third
party has no proof of who called. Plugins never receive session capabilities.
A proper design needs a trusted authenticated proxy or a short-lived proof
bound to an audience and purpose, with freshness and replay rules. It is
designed separately before any plugin depends on it.

Message middleware (filter, redact, route before delivery) is omitted until a
permission model exists; it can silently change what agents are told.

## Managed jobs: starting and waking sessions

Goal: an agent can ask zalki to start or wake a session for a target client, so
the session need not be running before it is messaged.

**This is a new execution authority, not a delivery detail.** Today
`authenticate` refuses retired or exited sessions, `Update` refuses revival,
and delivery has retirement and generation fences. Wake-by-resume must not
silently reactivate retired sessions, and an ordinary `send` to a retired
session keeps its existing contract (`unavailable`). It is a separate,
explicit, opt-in operation (a `managed job`) with its own state that
distinguishes the native conversation identity, the current run and process,
and the zalki session generation. Resumes of the same thread are serialized by
a cross-process lease.

**Safety is the design.** Off by default; gated by a user-owned policy file
(allowed kinds, working directories, concurrency, depth and fan-out). A spawn
or wake request arriving in a message is never user approval.

- **Job supervisor.** The exit observer is not enough: it discovers targets
  through watches, emits ended notices and is owned by an MCP process context.
  It keeps no durable child registry, does not reap owned children, and has no
  cancellation or orphan recovery. (Codex has no owned per-thread PID today.)
  Add a supervisor with durable launch intent, readiness and result records,
  PID plus start identity where meaningful, timeout and cancel rules, and
  recovery after a supervisor crash. Never kill a shared native daemon on a
  session's behalf.
- **Budgets are reserved globally before launch.** Record launch ID, root,
  parent actor, depth under the journal transaction, reserve fan-out and
  concurrency idempotently across all MCP processes sharing the state
  directory, then execute outside locks. Release a reservation only after
  proving launch did not occur or the run ended. An unknown launch retains its
  reservation until reconciled; blind rollback can exceed the global budget.
- **Credentials and fields.** The child gets its own zalki capability; strip
  parent zalki credentials from its environment; select cwd, native config
  and auth profile, and sandbox, tool and approval policy explicitly. Use
  distinct `parent_actor`, `root_job` and `initial_message` fields:
  `reply_to` already means a message UUID. Synthetic initial prompts and
  resumes must not count as human-origin resets of the loop guard.
- **Discovery.** A `can_spawn` flag on `list_agents` rows cannot advertise an
  installed provider that has zero sessions; discovery needs a provider listing.

**Job outcomes.** Distinguish *enqueued*, *woken* (an authenticated run is
ready) and *turn completed*, with bounded, version-specific evidence for each.
A hook firing alone does not prove that the intended generation is ready.
These are proposed job outcomes, not replacements for existing message
receipt statuses. A completed model turn is not the same as the requested
task succeeding; a process exit code alone does not establish either.

**Evidence per client.** Headless modes and resume-by-ID are documented for
Claude Code ([headless](https://code.claude.com/docs/en/headless)), Codex
([non-interactive](https://learn.chatgpt.com/docs/non-interactive-mode)) and
Cursor ([CLI](https://cursor.com/docs/cli/overview); its SDK has a different
approval and hook surface). Only Codex has been tested here.

*Reported Codex 0.167.0 experiment in a disposable, OS-sandboxed home
with no change to the user's configuration; a version- and surface-specific
result, not proof for other clients, the desktop app or current adapter
contracts:*

- A thread created by `codex exec` resumes by exact ID in a separate process
  and from a different cwd; set the cwd explicitly. Capture the ID from
  `--json` (`thread.started`), not rollout paths; do not use `--ephemeral`.
- Hooks fire in headless runs: `SessionStart` (source `startup` or `resume`),
  `UserPromptSubmit`, `Pre/PostToolUse`, `Stop`, `SessionEnd`; `SessionEnd`
  fires after every one-shot run. `PermissionRequest`, compaction, interrupt
  and subagent events were not exercised.
- A fresh run is read-only with approvals `never`; pin `--sandbox` and
  `approval_policy` explicitly each time (`--full-auto` is removed). The hook
  field `permission_mode` can read `bypassPermissions` on a read-only run, so it
  is never a sandbox verdict. The process exits 0 even when the model's shell
  command failed, and a restricted-network run waits until the caller's
  deadline, so every wake needs a deadline and result inspection.
- A thread open in another Codex TUI cannot be resumed independently
  (`thread-store conflict: ... already has an active writer`). So the adapter
  chooses per target: loaded, deliver through the existing backend; unloaded,
  wake by resume. An active-writer error is never a reason to start a
  duplicate worker.
- `codex queue --thread` is a **durable enqueue, not a wake**: it succeeds with
  no running session and a later resume receives the input.
  `codex queue --resume` additionally tries a resume through a daemon or
  `--remote` server, but only warns and still exits 0 when that fails.

*Still to test before building (record exact binary, argv, environment and
config isolation, redacted raw events and untested events):* Claude Code and
Cursor equivalents (hooks in headless runs, the permission flags that avoid a
hang, directory scoping of sessions, Cursor's lack of a non-interactive chat
list so the ID must be recorded at spawn), and the Codex desktop app.

## Phasing

| Phase | Content | Risk | User-visible |
| --- | --- | --- | --- |
| 1 | Migration steps 0 to 7, each gated by characterization tests | Medium; the work is design, not file moves | None |
| 2 | Loop guard; managed-job authority, supervisor and policy design | Design only | None |
| 3a | One scoped metadata event subscription with recoverable outbox, snapshot, public schema/version policy, fixtures, conformance kit and a reference subscriber example; four workflow plugin designs shown feasible, none built | Medium; new persistence and authorization contract | Documented protocol, new CLI subcommand |
| 3a+ | Scoped send as a separate grant | Medium | Grant management |
| 3b | Identity-proof design; limited declarative adapters; process adapters | Medium to high; new trust boundary | `zalki plugin` commands |
| 4 | Managed jobs (start or wake), per client, experimental | High; executes processes | New MCP tool, off by default |

## Design defaults

This proposal adopts these defaults. Recording the design does not itself
authorize installation, grants, process execution or production-code changes.

1. Third-party authoring is in scope, under the trust and public-contract
   requirements above.
2. Managed jobs are off by default behind a user-owned policy file.
3. The first managed-job implementation is headless only; interactive terminal
   panes are deferred.
4. Waking a retired session is a separate explicit operation and never appears
   as an ordinary `send`.
