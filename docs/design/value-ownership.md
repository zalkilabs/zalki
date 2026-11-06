# Value ownership for the bus refactor

Status: design for review; no implementation or schema change is authorized by
this document. Every **owner**, **bridge** and **migration step** below is a
proposal. Definitions, consumers and existing behavior are observations.

## Audited baseline and method

Audited fetched `origin/main` at
`16b1980840d6a885f626b2df3b23f3d4fe83911e` (2026-11-06), in a separate
worktree. [Source tree at that exact revision](https://github.com/zalkilabs/zalki/tree/16b1980840d6a885f626b2df3b23f3d4fe83911e).
The companion [architecture proposal](https://github.com/zalkilabs/zalki/pull/26)
is not a merged file at this baseline. Its migration step names, rather than
unmerged implementation or tests, determine the scope here. The separate
[characterization PR](https://github.com/zalkilabs/zalki/pull/27) is additional
coverage for review, not an assumed dependency of this inspection.

Citation prefixes resolve from the repository root at the audited revision:
`B/` = `internal/bus/`, `A/` = `internal/adapters/`, `I/` = `internal/install/`,
`M/` = `internal/mcpserver/`, `C/` = `cmd/zalki/`. For example,
`B/model.go:14` means `internal/bus/model.go`, line 14, at the SHA above.

Evidence was collected with Go 1.26.10: `go list` for actual imports and selected
files; `go/parser` and `go/ast` over production Go files for declarations,
receiver methods and imported selectors; and targeted source reads for private
state, inferred return values and locking. The AST scan included both Darwin
and Linux files, excluded tests, and was syntactic, not a whole-program pointer
or field-use analysis. The consumer appendix lists direct named-type references;
important consumers through inferred values are called out separately.

Reproduce the package checks without writing to the default Go build cache:

```sh
git rev-parse HEAD
GOCACHE=/tmp/zalki-ownership-go-cache go list -f '{{.ImportPath}}: {{join .Imports " "}}' ./...
GOCACHE=/tmp/zalki-ownership-go-cache GOOS=linux GOARCH=amd64 go list -f '{{.ImportPath}} {{join .GoFiles " "}}' ./internal/bus
rg -n '^type |^func .*\) (Error|Unwrap)\(' internal/bus internal/install internal/adapters internal/mcpserver
rg -n '\.(cause|validate|valid|canAcknowledge|move|hold|settleNotSent|releaseHeld|emitContext|applyReceipt)\b' internal/bus
rg -n '\bcause\s*:' internal/bus
```

These are inspection commands, not evidence that future extraction compiles or
that all characterization tests already exist. No unmerged tests are assumed.

## Ownership map

`model` below contains values and their own rules, with standard-library imports
only. `agent` contains the in-process adapter contract and registry and may
import `model`. `procwatch` takes raw OS values only; it imports neither of
those packages. `journal` supplies generic IO beneath the transaction and
imports neither `bus` nor the adapter contract. `bus` retains authentication,
policy selection, transactions and recovery.

### Process, liveness and endpoint values

| Value and definition | Proposed owner; dependency effect | Private behavior or required bridge | Encoding and consumer sensitivity |
| --- | --- | --- | --- |
| `ProcessRef`, `B/process.go:11` | `model`; `Session`, diagnostics and runtime records all refer to `model.ProcessRef`, rather than importing `procwatch` | No private fields/methods. Keep `ownerFrom(AdapterDescriptor, ...)`, `nativeOwner` and `rememberOwner` in `bus` (`B/process.go:45`, `:56`, `:78`). Project PID/start into a separate raw `procwatch.Identity`; role selection stays in `bus` | Preserve `pid`, `start`, `role`, including the platform-specific start fingerprint. Direct consumers: `B/model.go:37`, `B/diagnostics.go:18`, `B/runtime.go:24`, `B/exit_observer.go:48`, and the exit-source implementations |
| `processInfo`, `B/process.go:17` | `procwatch`, exported as `Info`; `bus -> procwatch`, never the reverse | Existing fields are already exported. Export raw `Read(pid)` and `Named(name)` equivalents of `readProcess`/`namedProcesses` (`B/process_darwin.go:18`, `:34`; `B/process_linux.go:12`, `:45`). A temporary `bus.processInfo` alias and `readProcess` wrapper can preserve reader-injection tests | No JSON contract today. Keep UID, PPID, name, zombie status and start identity; reducing it to PID would discard current evidence |
| `Liveness` and its three constants, `B/liveness.go:11` | `model`; shared by session records, diagnostics, runtime and adapters | No methods. Keep `processLiveness`, `probeProcess`, `processPresence`, adapter dispatch and result normalization in `bus` initially (`B/process.go:25`, `:27`; `B/liveness.go:53`, `:80`). They consume raw `procwatch.Info` and translate evidence to domain liveness | Preserve `alive`, `exited`, `unknown`; unknown is not exit. `A/claude/liveness.go:19`, `A/codex/liveness.go:13`, `B/discovery.go:15`, `B/runtime.go:29` consume it |
| `LivenessView`, `B/liveness.go:21` | `agent -> model` | No private members; `Endpoint` and `Owner` become pointers to model values. Its construction and lifetime remain in `bus.sessionLiveness`. It is credential-free, but not a deep-copy guarantee: the current view holds pointers | In-process SPI, no JSON tags or current serialized contract. Used with `LivenessProbes` by Claude/Codex and `LivenessAdapter` |
| `LivenessProbes`, `B/liveness.go:28` | `agent -> model` | Function fields belong to the host contract, not the persisted model. `bus.livenessProbes` supplies endpoint checking and PID-presence callbacks (`B/liveness.go:71`); adapters continue requesting probes through these callbacks rather than owning raw OS readers | Cannot become an RPC/JSON DTO unchanged. Preserve callback signatures and the presence rule that a live PID alone is not `alive` |
| `Endpoint`, `B/model.go:7` | `model`; the value has no OS dependency | Keep `InspectEndpoint`/`CheckEndpoint` IO in `bus` initially (`B/endpoint.go:10`, `:37`). Inject narrow inspect/check functions into clients and Cursor's listener port before banning client imports of `bus`; do not make `model` call back into `bus` | Preserve `path`, `device`, `inode`, `changed_ns` and value equality. Used by `HookRegistration`, `LivenessView`, `Session`, `ListenerLease`, Claude's socket-PID parser and endpoint functions. Private-directory, same-UID, symlink and socket-replacement checks remain behavior |
| `exitSource`, `exitEvent`, `exitEventKind`, `processKey`, `B/exit_source.go:18`, `:45`, `:37`, `:55` | Raw source/event/key values in `procwatch`; the consumer's orchestration remains `bus` | Publish a raw source API with PID/start identities and exported event kinds. Convert at `WatchProcess`/`Unwatch` and event-key lookup. Move `processGone` with OS implementations. The observer still owns session associations, journal fences, notices and source lifetime (`B/exit_observer.go:40`, `:128`) | No persisted protocol. Preserve nonblocking producer behavior, concurrency safety, close semantics and both platforms' post-registration/open fingerprint checks (`B/exit_source_darwin.go:86`; `B/exit_source_linux.go:92`) |

An important file-level trap: `changedNS` in `B/process_stat_darwin.go:5` and
`B/process_stat_linux.go:5` is used by **endpoint inspection and binary digest
identity**, not by `readProcess` (`B/endpoint.go:34`, `B/runtime.go:62`). Leave
these helpers with those consumers in step 2. Their filenames are not an
ownership argument.

### Persisted session and message values

| Value and definition | Proposed owner; dependency effect | Private behavior or required bridge | Encoding contract |
| --- | --- | --- | --- |
| `InboxMode`, `DeliveryTiming` and constants, `B/adapter.go:12` | `model`; neither `ReceiveCapabilities` nor `Message` may depend on `agent` for their field types | No private behavior. Keep all constants with the types; `Registry` imports them from `model` | Preserve `native`, `journal`, `next_turn`, `between_tools`, `next_hook`, `native_notification`. They feed stored receive data and public discovery/receipt output (`B/discovery.go:17`; `M/server.go:56`) |
| `ReceiveCapabilities`, `B/model.go:56` | `model`; removes an otherwise backward `model -> agent` edge | Move private `validate` (`B/adapter.go:146`) with the value, expose `Validate() error`, and switch registry callers before aliasing. Preserve the declared-delivery, inbox, hook-delivery and timing checks | Keep all nine exported JSON fields and their current tags, including explicit false values. `Session.Receive`, diagnostics and all three adapters use it; do not treat fields as removable just because selector searches miss inferred uses |
| `ListenerLease`, `B/model.go:50` | `model`; distinct from the live `Lease` resource | No methods. `bus.Lease` owns authentication, generation/lease-ID/endpoint fencing and persistence; moving this record does not move those rules | Preserve `id`, `generation`, `endpoint`. A non-nil listener requires session schema **2**; schema 1 is used again when no listener remains (`B/store_records.go:14`, `:20`, `:43`; `B/listener.go:293`) |
| `Session`, `B/model.go:14` | `model`, after its state/identity/receive/endpoint prerequisites | No receiver methods/private fields. Keep `Store` readers, schema selection, credentials, adapter-state validation, registration and retirement in `bus`. Move the pure `NativeID` projection (`B/session_names.go:18`) or retain a forwarding free function, not a foreign receiver method | Preserve every exported field/tag, `omitempty`, `json.RawMessage` state, and `KeyHash`. It is a persisted record, **not** a plugin or discovery DTO. `sessionFields` reflects it to strip core fields before adapter migration (`B/adapter_state.go:38`) |
| `SessionState`, `DeliveryStatus`, `B/status.go:11`, `:9` | `model`; all state constants and predicates travel together | Expose `Valid()` and `CanAcknowledge()` for current private `valid`/`canAcknowledge` callers; expose a narrow pending-status predicate for `bus`. Keep transaction admission/IO outside `model` | Preserve every string constant. `queued_for_approval` is response-only and remains invalid for persistence (`B/status.go:34`, `:41`). Pending, emitted, final refusal and uncertain submission must remain distinct |
| `Receipt`, `B/receipt.go:15` | `model`, after `DeliveryStatus` and `ErrorCode` | Move `NewReceipt` and `WithCause`. **The public setter already exists** (`B/receipt.go:10`); add a nonserializing `Cause() error` accessor for reads in `B/service.go:125` and `B/message.go:190`. Replace the private-field literal at `B/service.go:60` with `WithCause` before moving the type. Keep `cause` private | Preserve `at`, `status`, `code`, `error`, `output`, and omission rules. Never export a cause field without `json:"-"`: generic persistence/encoding must continue excluding the live error |
| `Message`, `B/model.go:68` | `model`, with status behavior and receipt; `bus -> model`, not `model -> bus` | Move `move`, `hold`, `settleNotSent`, `releaseHeld`, `emitContext`, `applyReceipt`, `IsQueuedForApproval`, and `transition` together (`B/status.go:62`, `:83`, `:87` onwards). Keep `move` internal; expose only the named operations that `bus` calls. Preserve the copy-before-save pattern in `Store.Finish` (`B/message.go:176`) | Preserve IDs/generation, routing, body, sender snapshots, held flag, nested receipt, ACK timestamp and all existing tags. `BusyDelivery`/`BodyPreview` stay `json:"-"`; body omission/defer flags remain response metadata rather than stored-body erasure |

The minimal message bridge is exported `Hold`, `SettleNotSent`, `ReleaseHeld`,
`EmitContext` and `ApplyReceipt` methods with the current semantics. Merely
exporting a generic setter for `Status` would discard transition checks. The
existing timestamp helper uses a **fixed nine-digit fraction**
(`B/identity.go:31`); a moved receipt/message helper must preserve that format,
not substitute variable-width `time.RFC3339Nano`. A later pure-rule API can take
an explicit time value, but changing clock sampling is not required for the
initial ownership move.

Capabilities are not just decoded and trusted from the record: bus re-evaluates
them through the adapter on session read and save (`B/session.go:67`, `:89`).
That recomputation remains in bus even though the capability value moves to
model. Moving it into a Session method would import the registry back into the
model.

### Adapter contract values

| Value and definition | Proposed owner; dependency effect | Behavior/bridge and consumers | Protocol sensitivity |
| --- | --- | --- | --- |
| `AdapterDescriptor`, `B/adapter.go:24` | `agent` | Registry and the three adapter `Descriptor` methods consume it; `bus.ownerFrom` also consumes process names/role (`B/process.go:56`). That policy remains in `bus`, which imports `agent`; `procwatch` never accepts a descriptor | No JSON tags/current persisted encoding. Exported members are an in-process SPI; hooks, kind, label, process names and owner role are behavior, not dead declarations |
| `HookInput`, `B/hook.go:13` | `agent` | Hook decoding/capture and watch notification remain in `bus` (`B/hook.go:108`; `B/adapter_contract_capture.go:14`; `B/watch.go:164`). Adapter normalization still happens after native contract capture and mapping | Preserve all native JSON names, including `hook_event_name`, `conversation_id`, `workspace_roots`, `status`, `loop_count`, pointer `session_title`, and optional `last_assistant_message`; these are input protocol fields |
| `HookRegistration`, `B/adapter.go:50` | `agent -> model.Endpoint` | Claude returns a bound endpoint; Codex sets `PreserveGeneration` for compact (`A/claude/claude.go:63`; `A/codex/codex.go:82`). Core registration remains in `bus` (`B/hook.go:144`). See endpoint-injection bridge above | In-process result, not a persisted DTO. Do not accidentally JSON-encode it to define a new plugin protocol |
| `SessionView`, `B/hook_lifecycle.go:15` | `model` | Data only. Keep `viewOf` in `bus` to copy raw adapter state (`B/hook_lifecycle.go:78`); Cursor policies and `Lease.Confirm` consume it. This choice lets listener interfaces and lifecycle SPI share the same view without importing `bus` | No current JSON tags. It excludes credentials and the builder detaches `AdapterState`; preserve both properties. It is not interchangeable with `Session` |
| `StartupContext`, `B/adapter.go:45`; `RecoveryGrant`, `B/hook_lifecycle.go:51` | `agent` | `bus` creates grants/output context; Cursor consumes them (`A/cursor/cursor.go:163`; `A/cursor/cursor_guidance.go:10`). No private fields, but ownership does not authorize additional callers | These carry plaintext credentials in-process. Preserve `RecoveryGrant.Capability`'s `json:"-"`; do not make these general discovery/event DTOs |
| `LifecycleTransition`, `LifecycleChange`, `RecoveryDecision`, `B/hook_lifecycle.go:24`, `:37`, `:44` | `agent -> model.SessionView` through host signatures | Cursor supplies pure policies; `hookRuntime` remains `bus` and enforces event-scoped transitions, copying, bounds and persistence under the lock (`B/hook_lifecycle.go:127`, `:161`) | In-process enum/results; integer order and callback signatures are current SPI. They are not an RPC schema or permission grant |
| `StateInput`, `B/adapter_state.go:15` | `agent` | Adapter codec contract; its sanitizing/copying builder and state-size rules remain `bus` (`B/adapter_state.go:62`, `:83`). Cursor consumes it in `A/cursor/cursor_state.go:61` | Raw JSON fields are copied and core session fields excluded. Moving the struct must not expose `KeyHash`, identity or other core fields to codecs |
| `ListenerStatus`, `B/listener.go:14` | `model` | Data enum for `agent.HealthHost`; `bus.listenerStatus` still combines endpoint and lifetime-lock evidence (`B/listener.go:95`). Cursor consumes constants through the host return value (`A/cursor/cursor.go:141`) | Preserve `none`, `running`, `stale`, `unleased`; do not merge legacy lock-only evidence into a confirmed lease |
| `SessionDiagnostic`, `B/diagnostics.go:10` | `model` | Required by `RecoveryHintAdapter` (`B/adapter.go:70`), Claude/Cursor recovery hints, and bus diagnostic assembly. Leaving its type in `bus` while moving that interface creates `agent -> bus` | Preserve all existing diagnostic JSON names, including `owner`, `receive`, transport/listener health and build fields. No private members; assembly/IO stays `bus` |

The interfaces `Adapter`, `NameAdapter`, `HookAdapter`, `HealthAdapter`,
`RecoveryHintAdapter`, `CompatibilityAdapter`, `LivenessAdapter`,
`SessionStateAdapter`, `SessionStateMigrationAdapter`, `HookHost`, `HealthHost`
and `LifecycleHost`, plus `StandardHooks`, `StandardEvents` and `Registry`,
belong in `agent` after their referenced values are available. Definitions:
`B/adapter.go:33`, `:40`, `:56`, `:64`, `:70`, `:76`, `:80`, `:162`, `:176`;
`B/liveness.go:39`; `B/adapter_state.go:23`, `:29`;
`B/hook_host.go:7`, `:13`; `B/hook_lifecycle.go:62`.
`hookRuntime`, callback execution and all `Store` methods stay in `bus`.

### Application requests, errors and record helpers

| Value and definition | Proposed owner; dependency effect | Behavior/bridge and consumers | Encoding sensitivity |
| --- | --- | --- | --- |
| `SendRequest`, `ReplyRequest`, `B/service.go:12`, `:17` | `bus`; these are authenticated use-case inputs, not adapter or model dependencies | Used by service/message reservation/reply orchestration, MCP handlers (`M/tools.go:142`, `:153`) and diagnostic send (`C/session.go:151`). No private members. Nothing in `agent` needs to import them | No tags, not persisted. They contain a capability and must not become log/event payloads or plugin credential carriers. Existing CLI/MCP input DTOs keep their separate field contracts |
| `ErrorCode`, `Error`, `B/status.go:148`, `:163` | `model`; adapters, receipts, status rules and MCP can use the same domain errors without `bus` imports | Move `Error()` and `ErrorCodeOf` (`B/status.go:168`, `:172`). Keep a `bus.fail` free-function wrapper for callers; `model` owns its equivalent constructor. Cursor already constructs the exported type (`A/cursor/cursor_state.go:81`) | Preserve code strings, `Error()` text prefix and `errors.As` identity through a type alias; preserve OS not-found/permission classification. `Error` has no JSON tags; MCP deliberately maps into `ToolError`, it does not serialize this error blindly |
| `stateFormatError`, `B/store_state_formats.go:26` | `bus` | Private `cause`, `Error`, `Unwrap`, `IsUpgradeRequired` and the state-wide write fence remain together. Journal failures are translated by bus, not by importing bus into journal | Not a JSON value. Keep upgrade-required distinguishable from an individual unsupported/unreadable record; both can have `unsupported_schema` without having the same operational meaning |
| `NoticeDeliveryError`, `B/watch.go:271` | `bus` | `Err`, `Error` and `Unwrap` wrap best-effort notice delivery separately from journal observation errors. CLI checks the concrete type (`C/diagnostics.go:93`) | Not persisted. Preserve `errors.As`/unwrap behavior and the distinction that a failed notice need not hide diagnostic results |
| `M/ToolError`, `M/server.go:20` | `mcpserver` (outside the extraction) | Its `Code` becomes `model.ErrorCode`; response conversion stays at the MCP boundary | Preserve `code`, `message`. It is a protocol DTO, not the domain `error` implementation |
| `recordUpdate`, `B/store_record_merge.go:172`; `replyTransaction`, `B/message_reply.go:16`; `stateFormats`, `B/store_state_formats.go:12` | `bus` | Keep private intent preservation data, obsolete-field selection, reply identity/recovery, family lists and the high-water mark in the transaction owner. Journal may receive raw previous JSON/removed-key options, never a `Store` | `recordUpdate` is private/unserialized; reply intents and marker schemas/keys are durable protocols. Generic merge code must preserve unknown fields and case-folded ownership (`B/store_record_merge.go:64`, `:121`, `:180`) |
| Proposed generic codec size/error values | `journal` | `encodeStateJSON` currently calls `bus.fail(CodeInvalidInput, ...)` (`B/store.go:108`). A neutral exported limit error carries byte count/limit; the bus wrapper restores the exact domain code/text. Keep domain schema checks in bus or translate a neutral schema error there | Preserve the 128 KiB bound **including the newline**, before marker publication; preserve JSON errors, formatting, unknown fields and omission/clearing behavior |

Other current application records/results can remain in `bus` throughout steps
2–6; crossing the MCP/application boundary alone does not require relocation:

| Values and definitions | Production consumers; reason to remain |
| --- | --- |
| `AgentInfo`, `B/discovery.go:7` | Bus discovery plus `M/server.go:153`, `:160`; projection deliberately omits native IDs, adapter state, credentials and endpoints |
| `AgentDiagnostic`, `DiagnosticReport`, `B/diagnostics.go:37`, `:28` | Bus diagnostic assembly and inferred CLI output; the adapter contract needs only `SessionDiagnostic` |
| `RuntimeInfo`, `RuntimeStatus`, `RuntimeLease`, `B/runtime.go:18`, `:203`, `:123` | Runtime tracking, diagnostics, service and `M/server.go:16`; live lease owns private `store/info/mu/closed`, rather than moving with process identity. CLI retains runtime tracking around Cursor Listen (`C/session.go:100`) |
| `MessageQuery`, `MessageBatch`, `B/message_inbox.go:17`, `:29` | Bus query use case plus `M/tools.go:168`, `M/server.go:126`; query carries credentials, batch contains model messages. No adapter interface needs either |
| `SessionIssue`, `B/store_records.go:88`; `PruneItem`, `PruneReport`, `B/prune.go:12`, `:16` | Bus scanning/prune and inferred CLI output; keep selection, dry-run reports and deletion orchestration in the application layer |
| `Watch`, `WatchResult`, `B/watch.go:35`, `:26`; `TaskResult`, `B/task.go:13` | Bus watch/task use cases plus `M/tools.go:179`, `:185`; retain stored watch schema and public result tags |

All exported JSON fields in these remaining DTOs remain exported. Trim work must
check reflection, JSON tags and interface method sets, not only named-type uses.

## Cursor Listen and the live Lease

`cursor.Listen` directly takes a `bus.Store` (`A/cursor/listen.go:37`). Its lease
is inferred from `Store.AcquireListenerLease`, so a search only for `bus.Lease`
misses this dependency. Existing `deliveryLease` (`A/cursor/listen.go:25`) covers
only hint consumption; it does not cover acquisition, arm, confirmation or
cleanup.

**Proposal:** keep the concrete `Lease` in `bus` (`B/listener.go:35`). Its private
`store`, `actor/kind/key/id/gen`, `fd`, `endpoint`, `confirmed/closed` form one
live authority/resource. Do not export those fields or place the concrete lease
in `model`, `agent`, `journal` or `procwatch`.

Define the full consumer-owned `ListenerHandle` interface in the Cursor
package. It needs the following existing operations, with model value types
after step 5:

```go
type ListenerHandle interface {
    Generation() string
    Arm(*model.Endpoint) error
    Confirm(func(model.SessionView) (json.RawMessage, error)) error
    ValidateDelivery(string) (bool, error)
    RejectHint(error)
    WakeEmitted(string)
    Close(error)
}
```

Inject acquisition as `func(actor, key string) (ListenerHandle, error)` and
inspection as `func(string) (*model.Endpoint, error)` into `Listen`. The CLI
composition closure calls `store.AcquireListenerLease`; it retains its existing
runtime tracking. In step 4 the interface may still spell the value parameters
as `bus.Endpoint`/`bus.SessionView`; their later aliases preserve identity until
the import is switched. The interface belongs to its Cursor consumer, not the
core registry.

**Go method sets matter:** a method returning `*bus.Lease` does not satisfy an
interface requiring a method returning `ListenerHandle`. Return values are not
covariant. Use the explicit factory closure/wrapper; a compile assertion on
`*Lease` checks only the handle methods, not acquisition compatibility.

Preserve the actual lifecycle:

1. Acquisition authenticates under a journal transaction, releases it, then
   takes the nonblocking lifetime lock (`B/listener.go:110`, `:134`). There is
   no waiting for a lifetime lock while holding the journal lock.
2. Arm reauthenticates and checks generation, then records the bound endpoint
   (`B/listener.go:204`). The private `owns` predicate compares lease ID,
   generation and endpoint (`B/listener.go:195`).
3. Announce readiness **before** `Confirm`; that callback runs inside the
   journal transaction and its adapter state is validated before saving
   (`A/cursor/listen.go:71`, `:76`; `B/listener.go:232`). It is not an RPC call.
4. Validate each hint against generation, recipient, pending state and ACK,
   preserving approval/retirement fencing (`B/listener.go:261`).
5. Close clears only its own persisted lease and releases the lifetime fd
   (`B/listener.go:304`). Preserve idempotence and the single-goroutine rule.

`removeFreeListenerLock` does probe the lifetime lock while already under the
journal lock, but only nonblockingly (`B/listener.go:47`). The lock-order rule
must preserve this cleanup exception rather than mechanically prohibit it.

The same client-import cleanup needs endpoint injection for
`A/cursor/cursor_transport.go:17`, `:29` and for Claude registration, health and
both pre/post-dial checks (`A/claude/claude.go:67`, `:72`, `:86`, `:108`). A
listener port alone does not remove those `bus` imports. Wire inspected/check
functions in the CLI/composition layer and keep their current call order.

## ConfigurationAdapter and install.ConfigFile

Current facts:

- `ConfigurationAdapter.Configuration` exposes `[]install.ConfigFile`
  (`B/adapter_config.go:13`). All three clients implement it; registry
  aggregation also validates paths, duplicate draft names, and exactly one
  JSON/TOML representation (`B/adapter_config.go:18`).
- `install.ConfigFile` is not only a DTO: its private `target(home)` method
  resolves `ConfigRootEnv` (`I/config.go:13`, `:22`). Installation planning and
  setup invoke it (`I/installation_plan.go:130`; `I/setup.go:28`). Its `Path`
  remains the stable ownership key even when its basename is relocated.
- `install.Declare` requires a function returning `[]install.ConfigFile`
  (`I/installation.go:42`). Install, setup, uninstall and upgrade currently
  pass the `Registry.Configuration` method value (`C/install.go:47`, `:69`;
  `C/uninstall.go:21`; `C/upgrade.go:158`).

**Proposed split:** `model.ConfigDeclaration` owns the five pure declaration
fields (`Path`, `Draft`, `JSON`, `TOML`, `ConfigRootEnv`) at the SPI boundary;
`agent.ConfigurationAdapter` returns those declarations after step 6.
`install.ConfigFile` remains an installer-owned execution specification with
its private method. This is an explicit exception to assigning every current
type to one of the new packages: the installer type need not move at all.

Change registry aggregation to return the neutral declarations. A CLI
`install.Declare` closure converts **all five fields explicitly** into
`install.ConfigFile` before calling the unchanged installer. Keep JSON maps and
their current mutation behavior; a new copy/normalization policy would be a
separate behavior change. Registry validation must use an identical neutral
relative-path predicate rather than importing `install.SafePath` into `agent`.
Before implementation, decide whether it is shared through an allowed neutral
leaf or deliberately revalidated at both boundaries. Preserve equivalent
clean/relative/root-escape semantics on both platforms; do not silently fork
path validation merely to satisfy the import diagram.

This avoids `agent -> install`, `install -> agent` and `model -> install`.
Aliasing `install.ConfigFile` to a foreign type while leaving its private
`target` receiver in `install` would not compile. A same-shaped new type is not
assignable to the existing slices/function signatures without conversion.
There are no JSON tags on `ConfigFile` today; its generated JSON/TOML and the
ownership manifest, not automatic struct JSON, are the compatibility surface.
Keep the current installer-owned type until this boundary is deliberately
changed. If a temporary `agent -> install` exception is preferred instead,
record it explicitly; introducing a duplicate DTO is useful only when the
neutral SPI boundary is actually required.

**Remaining target-diagram discrepancy:** clients also call
`install.HookConfig`, `HookCommand`, `MCPConfig`, and `ShellQuote`
(`A/claude/claude.go:29`, `:30`; `A/codex/codex.go:30`;
`A/cursor/cursor.go:25`, `:29`, `:169`; `A/cursor/cursor_guidance.go:14`).
Changing the return DTO does not remove these imports. Recommendation: allow
this existing leaf helper dependency explicitly in the first refactor, keeping
the installer unchanged. A literal final rule of clients importing only
`agent`/`model`/listener port needs an additional helper extraction decision.
Neither silently copy the builders nor hide the dependency behind an alias.

## Other dependency bridges needed before moving the SPI

| Existing edge | Proposed bridge; evidence |
| --- | --- |
| `Registry` calls private model validation and name rules | Expose `ReceiveCapabilities.Validate` and pure kind/name validation from `model`; preserve the exact regex and name bound (`B/adapter.go:82`, `:91`, `:98`, `:105`; `B/session_names.go:12`). Bus path/prune validators also use the kind predicate; do not move the only copy into `agent` |
| Clients call `bus.UUID`, `NativeID`, `Envelope`, `NewReceipt` | Move/forward pure helpers into `model`, then switch client imports. `Envelope` depends on `validName` and private `noticeKind`; move their rule/value with it (`B/envelope.go:11`, `:12`, `:28`; `B/watch.go:19`). Keep `Store.withLabel` in `bus` because it resolves the registry |
| Codex handshake reads `bus.Version` | Pass build version into the Codex adapter/control helper from composition; `A/codex/codex_app_server.go:197` reads it for `clientInfo`. Preserve the existing release linker variable initially (`B/runtime.go:15`), rather than accidentally introducing an adapter-to-bus edge or changing release metadata |
| Bus tests use private value methods | Existing `B/status_test.go:58`, `:76`, `:80`, `:86`, `:89`, `B/approval_reservation_test.go:13` and `B/adapter_test.go:205` require relocation with the methods or conversion to the reviewed public bridges. A type alias does not grant same-package tests access to foreign private methods |
| `Registry.Configuration` is a method on the type being moved | Move all remaining registry methods with `agent.Registry` after neutralizing configuration dependencies, or convert them to free functions first. A `bus.Registry` alias cannot retain a newly declared receiver method (`B/adapter_config.go:18`) |

## Dependency graph and prospective cycles

`go list` shows the current production package graph is acyclic. Client
implementations import both `bus` and `install`; `bus` imports `install`,
`fsutil`, `logging` and platform OS packages. The composition package imports
`bus` and concrete clients, while `bus` does not import concrete clients.
The existing depguard rules explicitly allow `bus -> install`
(`.golangci.yml`, core/installer/composition rules).

The proposed steady-state graph, including the explicit helper exception, is:

```text
bus -> model, agent, procwatch, journal, logging
agent -> model
model -> standard library
procwatch -> standard library, x/sys
journal -> standard library, fsutil
concrete clients -> agent, model, install helpers, consumer listener interfaces
CLI/composition -> bus, agent, concrete clients, install
install -> fsutil, TOML library, standard library   (unchanged)
```

The CLI converts configuration declarations and supplies endpoint/listener/build
dependencies; it does not move credential-bearing `Store` access into clients.

| Tempting move | Cycle or compile failure | Required break |
| --- | --- | --- |
| Move SPI while its signatures still name `bus.Session`, `SessionDiagnostic`, views or hosts | `bus -> agent -> bus` | Move referenced values first; keep implementations in bus and interfaces in agent |
| Put receive enums/capabilities in agent while moving Session/Message to model | `model -> agent -> model` | Persisted capability values and enums belong in model |
| Alias model process identity to procwatch while procwatch uses model liveness | `model -> procwatch -> model`, also violates both leaf contracts | Separate raw OS identity from the persisted role-bearing value; translate in bus |
| Move live Lease with its private Store into agent/model | `bus -> agent/model -> bus` | Keep the resource in bus, expose a consumer-owned interface |
| Move codecs/locks and import bus for `Store`, `fail` or recovery | `bus -> journal -> bus` | Neutral IO/errors beneath a bus-controlled transaction |
| Move a type but leave receiver methods on an old-package alias | Illegal foreign receiver, even without an import cycle | Move methods together; wrappers in the old package must be free functions |

## Compiling sequence for migration steps 2–6

These are proposed commit boundaries, not a claim that changes have been
implemented. Every row must compile, including package tests, before the next.
Existing characterization tests at the audited SHA are the starting point;
verify/add the required boundary tests before performing the relevant move.
Add necessary depguard import allowances in each introducing commit; step 6b
tightens the final boundaries rather than leaving earlier commits lint-broken.

| Commit within architecture step | Change and compile condition |
| --- | --- |
| **2a: raw procwatch leaf** | Introduce raw `Info`, PID/start `Identity`, source/events and exported read functions alongside the existing bus implementation. Copy the OS readers and exit implementations into the new leaf as temporary migration scaffolding; do not delete the bus definitions before step 2b switches their callers. Procwatch must not import bus/model/agent |
| **2b: bus conversion** | Switch the bus raw-reader wrappers and observer call sites to procwatch; retain domain `ProcessRef`/`Liveness`, owner selection, probes and observer orchestration in bus for now. Use explicit identity/event conversion, not a model-to-procwatch alias. Preserve a small bus source wrapper while observer tests use domain ProcessRef/event contracts, or switch the observer and fakes to the raw public API together. An alias alone cannot reconcile domain ProcessRef with a distinct raw Identity. Move raw mechanism tests with their implementation and retain observer tests in bus. Leave `process_stat_*` with endpoint/runtime consumers |
| **3a: generic journal primitives** | Introduce bounded read/encode/merge/atomic-write/sync/lock primitives with neutral errors. Bus wrappers translate size/schema errors into existing codes/text. No domain types or family lists are imported into journal; current bus operations continue compiling through wrappers |
| **3b: transaction integration** | Replace primitive calls beneath `Store.lockedMode`, keeping private-dir setup, one flock, write-compatibility check, recovery and domain work in their current order (`B/store.go:57`). Keep reply intent publication/application/removal and marker high-water publication in bus (`B/message_reply.go:80`, `:109`, `:177`; `B/store_record_merge.go:214`). Read paths may recover replies under the lock when compatible; they are not automatically side-effect-free |
| **4a: prove Cursor's port** | Introduce the consumer interface using current bus value names, add the acquisition/endpoint factory closures in CLI, and update Listen to accept them. This removes the concrete Store dependency without prematurely moving lifecycle policy/leases. Add endpoint injection to transport/Claude before the final client import ban |
| **4b: neutral configuration boundary** | Introduce only the independent `model.ConfigDeclaration` leaf after resolving validation ownership, change client Configuration/registry aggregation results, and adapt all four CLI installer entry paths and affected configuration tests with an explicit `install.Declare` conversion in the same commit. Preserve nil/empty results, map aliasing, sort order and validation errors. Keep install.ConfigFile and its target method unchanged. Remove registry's installer-helper dependency before moving that method |
| **5a: foundational model values** | Move errors/codes, state/timing/inbox enums, process/liveness/endpoint/listener values and receive capabilities with their predicates/validation. Add aliases, constants and free-function wrappers in bus. All bus callers and affected tests must use public predicate/validation bridges before old private methods disappear. Raw procwatch APIs still do not import model |
| **5b: receipts** | Add Cause accessor while Receipt is still in bus; replace both private-cause reads and the service.go:60 private-field literal using the existing WithCause setter. Then move the value, WithCause and constructor into model, preserving timestamp format and JSON omission. Bus keeps a constructor forwarding function for existing callers |
| **5c: sessions and messages** | Move Session after its field types. Move Message with the entire transition/held/context/receipt behavior in one commit, switching bus callers to the narrow exported operations and relocating/updating private-method tests. Move pure identity/envelope helpers with their name/notice dependencies. Add the bus aliases only when no forbidden private access remains |
| **5d: SPI snapshots and pure rules** | Move SessionView and SessionDiagnostic into model. Keep snapshot copying, callback invocation, sanitization, journaling and domain error translation in bus. Extract further pure approval/flood/prune decisions only as value-in/value-out functions; no model signature may mention Store, Registry or IO. Apply those results under the same bus transaction |
| **6a: agent contract and registry** | Move interfaces, descriptors, hook/state/liveness/lifecycle DTOs, StandardHooks/Events and Registry with all registry methods. Registry references only model and standard-library validation. Move no Store receiver or hookRuntime implementation. Bus aliases are legal for types, not a place to attach methods |
| **6b: close imports and enforce them** | Switch client contracts and selectors to agent/model, supply endpoint and build-version dependencies from composition, and replace client bus imports only after the residual-helper inventory is empty. Add the model/procwatch/journal/agent/client depguard rules in this same commit, explicitly recording the install-helper exception. Compile/test the whole module and both supported OS selections; do not execute Linux test binaries on Darwin |

There is no checkpoint where `model` imports `bus`, or where aliases are expected
to make private members accessible. The first model leaf in step 4b is a
prerequisite, not a premature move of the entire model. Broader pure-rule API
changes and eventual removal of compatibility wrappers should be separately
reviewable after the ownership boundary works.

## Direct consumer inventory

The compact map above includes the important boundary consumers. These are the
direct **named-type** production file references found by the AST scan,
excluding the type-name declaration itself. Each citation identifies the first
such reference in that file. They are not an inventory of every inferred field
access. Files that also define the type can contain references in methods or
other declarations.

| Value | Direct named-type references |
| --- | --- |
| `ProcessRef` | `B/diagnostics.go:18`; `B/exit_observer.go:48`; `B/exit_source.go:23`; `B/exit_source_darwin.go:19`; `B/exit_source_linux.go:21`; `B/liveness.go:23`; `B/model.go:37`; `B/process.go:25`; `B/runtime.go:24` |
| `Liveness` | `A/claude/liveness.go:19`; `A/codex/liveness.go:13`; `B/diagnostics.go:17`; `B/discovery.go:15`; `B/liveness.go:14`; `B/model.go:38`; `B/process.go:25`; `B/runtime.go:29` |
| `LivenessView` | `A/claude/liveness.go:19`; `A/codex/liveness.go:13`; `B/liveness.go:40` |
| `LivenessProbes` | `A/claude/liveness.go:19`; `A/codex/liveness.go:13`; `B/liveness.go:40` |
| `ReceiveCapabilities` | `A/claude/claude.go:38`; `A/codex/codex.go:42`; `A/cursor/cursor.go:39`; `B/adapter.go:35`; `B/diagnostics.go:25`; `B/model.go:45` |
| `InboxMode` | `B/adapter.go:16`; `B/model.go:59` |
| `DeliveryTiming` | `B/adapter.go:18`; `B/discovery.go:17`; `B/model.go:60` |
| `ListenerLease` | `B/listener.go:220`; `B/model.go:43` |
| `Lease` | `B/listener.go:110` |
| `Endpoint` | `A/claude/liveness.go:29`; `B/adapter.go:51`; `B/endpoint.go:10`; `B/listener.go:39`; `B/liveness.go:22`; `B/model.go:40`; `B/session.go:112` |
| `AdapterDescriptor` | `A/claude/claude.go:34`; `A/codex/codex.go:35`; `A/cursor/cursor.go:33`; `B/adapter.go:34`; `B/process.go:56` |
| `HookInput` | `A/claude/claude.go:63`; `A/codex/codex.go:82`; `A/cursor/cursor.go:78`; `B/adapter.go:57`; `B/adapter_contract_capture.go:14`; `B/hook.go:108`; `B/watch.go:164` |
| `HookRegistration` | `A/claude/claude.go:63`; `A/codex/codex.go:82`; `B/adapter.go:58` |
| `SessionView` | `A/cursor/cursor.go:218`; `A/cursor/listen.go:101`; `B/hook_lifecycle.go:64`; `B/listener.go:232` |
| `SendRequest` | `C/session.go:151`; `B/message.go:58`; `B/message_reply.go:50`; `B/service.go:29`; `M/tools.go:142` |
| `ReplyRequest` | `B/message_reply.go:30`; `B/service.go:162`; `M/tools.go:153` |
| `ErrorCode` | `B/logging.go:20`; `B/receipt.go:19`; `B/status.go:105`; `M/server.go:21` |
| `Error` | `A/cursor/cursor_state.go:81`; `B/status.go:168` |

- `Session`: `A/claude/claude.go:38`; `A/codex/codex.go:42`;
  `A/codex/codex_app_server.go:62`; `A/cursor/cursor.go:39`;
  `A/cursor/cursor_transport.go:14`; `B/adapter.go:35`;
  `B/adapter_state.go:41`; `B/approval.go:261`; `B/diagnostics.go:56`;
  `B/discovery.go:37`; `B/exit_observer.go:306`; `B/hook_host.go:13`;
  `B/hook_lifecycle.go:78`; `B/listener.go:79`; `B/liveness.go:53`;
  `B/message.go:58`; `B/message_inbox.go:81`; `B/message_reply.go:30`;
  `B/prune.go:121`; `B/service.go:26`; `B/session.go:59`;
  `B/session_names.go:18`; `B/store_records.go:34`; `B/watch.go:195`.
- `Message`: `A/claude/claude.go:42`; `A/codex/codex.go:61`;
  `A/codex/codex_app_server.go:135`; `A/cursor/cursor.go:48`;
  `A/cursor/cursor_transport.go:14`; `B/adapter.go:36`; `B/approval.go:16`;
  `B/envelope.go:11`; `B/flood.go:77`; `B/logging.go:89`; `B/message.go:13`;
  `B/message_inbox.go:13`; `B/message_reply.go:20`; `B/service.go:26`;
  `B/status.go:83`; `B/watch.go:234`; `M/server.go:35`.
- `Receipt`: `A/claude/claude.go:42`; `A/codex/codex.go:61`;
  `A/codex/codex_app_server.go:135`; `A/cursor/cursor.go:48`;
  `A/cursor/cursor_transport.go:14`; `B/adapter.go:36`; `B/message.go:168`;
  `B/model.go:97`; `B/receipt.go:4`; `B/service.go:26`; `B/status.go:101`.

For example, MCP reads receipt fields through `Message.Receipt`
(`M/server.go:37`) without directly naming the `Receipt` type. Cursor obtains
its lease without naming `Lease`. Such inferred uses are why a selector count
alone cannot justify unexporting fields or moving resources.

## Findings that qualify the architecture proposal

1. A receipt cause constructor/setter is not the missing API: `WithCause`
   already exists; cross-package **read** access is the missing bridge, and
   the existing service.go:60 struct literal must use that setter before moving.
2. Listener ports and type moves do not exhaust client-to-bus dependencies:
   endpoint checks/inspection, envelope/identity helpers, diagnostic types and
   the Codex build-version read also need an owner or injection.
3. Moving all files named `process_*` would move endpoint/runtime stat helpers
   unnecessarily. Move by dependency, not filename.
4. The target table's adapter imports cannot be exhaustive while installer
   helpers stay unchanged. The proposed neutral declaration conversion solves
   the SPI DTO problem, not the remaining helper-import policy.
5. “Lifetime lock before journal lock” describes active lease work; acquisition
   first authenticates and releases a transaction, while cleanup probes the
   lifetime lock nonblockingly under the journal lock. Preserve those cases.

## Implementation decisions and inspection limits

- Record the install-helper dependency exception, or separately choose a leaf
  package for configuration builders. This map recommends retaining the
  existing helper dependency for the first refactor, does not select another
  package by implication, and leaves the neutral path-validator owner explicit.
- Choose the exact public names/signatures of narrow message operations and
  the receipt cause accessor. Existing behavior is specified above; no new
  bypass of state rules is proposed.
- Decide whether a later adapter-specific diagnostic snapshot should replace
  the whole SessionDiagnostic SPI. Moving its current data value into model
  is the compiling first step; a smaller shape is a distinct contract change.
- Inspection did not prove that the complete characterization suite described
  by the architecture proposal exists or is sufficient. No new tests, runtime
  fault injection, Linux execution or refactor compilation was performed here.
  The migration gates must be verified on merged code before each actual move.
