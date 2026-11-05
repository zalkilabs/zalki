# Message-size investigation and adapter-limit proposal

Status: design only, based on `316a6ca`, investigated 2026-11-02.
No production body limit, transport, admission, journal, or hook code changed.

## Decision

Keep the current 16,384-byte UTF-8 body limit. A safe increase is not an
adapter-only change. It crosses bounded journal reads, reply recovery, MCP and
CLI framing, and Cursor's hook batching. Changing a constant or adding one
validation call would leave accepted messages unreadable or undeliverable.

Native capacity, raw body bytes, serialized frame size, hook batch size, and
model/context cost are different limits. The native Claude allowance is not a
portable one-megabyte body allowance for every Zalki transport.

The suspected Cursor head-of-line obstruction does **not** reproduce for a
body accepted by today's limit. It reproduces when that admission limit is
bypassed to insert a larger body. No current-limit Cursor fix branch is justified
by these fixtures.

## Current limits by layer

| Layer | Current bound | What it actually measures |
| --- | --- | --- |
| Service send/reply validation | 1–16,384 bytes; valid UTF-8; no NUL; capability text refused | Raw body bytes |
| Reservation validation | Duplicates the 16,384-byte/NUL guard | Raw body before target resolution |
| Claude adapter | At most 65,536 bytes | Fully JSON-encoded frame plus newline, including envelope/metadata |
| Claude Code 2.1.316 receiver | Destroys when its buffered string exceeds 1,048,576 units | JavaScript UTF-16 code units, before newline extraction |
| Codex app-server adapter | 1 MiB WebSocket **read** limit | Inbound responses/events, not an outbound body allowance |
| Codex queue fallback | Envelope passed as one `--message` argument | OS argv/environment limit plus an unverified native content limit |
| Cursor listener socket | 4,096-byte hint/response decoder | UUID/generation control data; the body stays in the journal |
| Cursor hook delivery | At most five messages and 24 KiB per batch | Raw UTF-8 envelopes before output JSON encoding |
| Journal `readJSON` | 128 KiB per regular state file | Entire encoded message, session, or reply-transaction record |
| Journal `writeJSON` | Same 128 KiB bound as `readJSON` | Exact indented encoding plus newline; oversized writes are refused before replacement |
| MCP server stdio | 128 KiB | A single **inbound** newline-delimited JSON-RPC frame |
| Diagnostic CLI send | 128 KiB reader | Encoded stdin JSON, not raw body bytes |
| Hook input decoder | 128 KiB | Incoming hook event JSON, not outgoing additional context |
| `list_messages` normal page | 24 KiB message-page budget | Serialized response views; not the whole mirrored MCP result |
| Exact message lookup | Complete body, no normal page truncation | May produce a much larger response |

Relevant code is in `internal/bus/message.go`, `service.go`, `store.go`,
`message_reply.go`, and `message_inbox.go`; `internal/mcpserver/server.go`;
`cmd/zalki/session.go`; and the owning adapter files. This branch changes none
of those files.

### Claude

The adapter's 64 KiB frame guard runs after JSON encoding. An otherwise valid
16 KiB body of U+0001 controls expands enough to fail that guard today; the
existing `TestClaudeFrameSizeAndMissingEndpoint` covers this. Raw body admission
is an upper bound, not a promise that every encoding fits every transport.

The installed Claude 2.1.316 binary is the one documented in
[`claude-delivery-ack.md`](claude-delivery-ack.md), SHA-256
`64c327e92b2c5e0f78c0a980c5a518121e1f0d140ca771ca5dd4488d1a2dcb3f`.
Its embedded `chunk-9w4aab70.js` sets `Hwn=1048576`; the inbox handler appends
decoded strings, checks `f.length > Hwn`, and only then extracts complete lines.

An offline evaluation of that same connection handler, with dispatch stubbed
and no native connection or model request, produced:

| Synthetic frame | UTF-16 units including newline | UTF-8 bytes | Dispatched / destroyed |
| --- | ---: | ---: | --- |
| ASCII at threshold | 1,048,576 | 1,048,576 | yes / no |
| ASCII one unit over | 1,048,577 | 1,048,577 | no / yes |
| Emoji at threshold | 1,048,576 | 2,097,110 | yes / no |

Dispatch here means reaching the stub, not admission by native peer policy or
model consumption. There is still no inline ACK. The public
[Claude message-size description](https://code.claude.com/docs/en/cross-session-messaging#limitations)
also describes the local cap in serialized characters. A future implementation
must budget the envelope, JSON escaping, newline, and authentication framing,
rather than compare raw UTF-8 body bytes directly with that number.

### Codex

The installed CLI reports `codex-cli 0.166.0`. Zalki submits the envelope in
`turn/start` as `toolOutput.output`, with empty `input`. The
[official OpenAI App Server documentation](https://learn.chatgpt.com/docs/app-server#turnstart)
describes this input shape but the reviewed reference establishes no maximum
byte length for that output string. Zalki's `codexFrameLimit=1<<20` feeds
`SetReadLimit`; `wsjson.Write` has no analogous adapter-side size check.
It would be incorrect to report that read limit as a verified one-megabyte
outbound capacity.

The fallback uses `codex queue --thread UUID --message ENVELOPE`.
`codex queue --help` documents the string argument, without an explicit content
limit or an stdin/file replacement. No live queue request was made.

On this macOS host, `getconf ARG_MAX` returned 1,048,576. Executing the harmless
`/usr/bin/true` with an empty environment and one ASCII argument of 16,384,
65,536, or 262,144 bytes succeeded. An argument of 1,048,576 bytes failed with
`E2BIG` / “Argument list too long”. The real queue has additional arguments and
an inherited environment, so its available space is smaller and host-dependent.
This probes OS exec only; it does not verify Codex's native parser or model limit.

Any future Codex body allowance must fit **both** verified app-server delivery
and queue fallback, or refuse an unsafe fallback before it is attempted. A large
app-server write cannot be retried through the queue after uncertain acceptance.

### Cursor

The [Cursor hook reference](https://cursor.com/docs/hooks#posttooluse) describes
`additional_context` as a string, but the reviewed page establishes no numeric
native output limit. Stop uses `followup_message` in Zalki; PostToolUse uses
`additional_context`. Do not turn the absence of a documented bound into a
claim of unlimited native context.

The listener exchanges only message ID and generation, then prints a
`ZALKI_WAKE UUID` hint. Its 4 KiB decoder is not a body limit. The receiving
agent retrieves the canonical body from MCP/journal, so larger bodies would
still meet the journal and exact-read response constraints.

For hook fallback, `EmitPending` compares raw `Envelope` byte lengths with
24 KiB. JSON encoding happens later in the adapter. It must not be confused
with the encoded hook-output length.

To check the suspected current-limit obstruction, temporary fixtures used
actual `Service.Send` admission and `EmitPending`, then inspected both message
statuses. The sender had the allowed 512-byte label and an 81-rune nonprinting
name, which the envelope truncates to 80 runes. Each 16,384-byte large message
was followed by a small message:

| Valid body | Raw two-message batch bytes | Encoded hook bytes | Both statuses |
| --- | ---: | ---: | --- |
| ASCII | 19,311 | 24,637 | `hook_context_emitted` |
| U+0001 controls | 19,311 | 106,557 | `hook_context_emitted` |
| Non-BMP emoji | 19,311 | 24,637 | `hook_context_emitted` |
| U+2028 line separators | 19,311 | 41,020 | `hook_context_emitted` |
| HTML characters | 19,311 | 106,557 | `hook_context_emitted` |

All bodies passed today's validation. Every large and later small message was
emitted. These are core/fake-adapter observations, not proof that native Cursor
accepts every encoded hook size.

By contrast, inserting a **24,576-byte** ASCII body directly into an isolated
journal, bypassing today's 16 KiB admission, produced a 24,725-byte envelope.
`EmitPending` returned zero bytes, left that message pending, and also left the
later small message pending. That is a blocker for a future increase, not a
current-limit reproduction.

## Encoding and inbox measurements

Temporary Go probes used the actual `bus.Message`, `Envelope`,
`json.MarshalIndent`, journal write/read, and `messagePage` implementations.
Fixtures used fixed-length IDs and a short sender label. Real metadata changes
the overhead; these are measured examples, not universal maximums.

| Body | Journal bytes | Claude frame bytes | MCP send line bytes | Journal readable today |
| --- | ---: | ---: | ---: | --- |
| 16 KiB ASCII | 16,750 | 16,671 | 16,672 | yes |
| 16 KiB controls | 98,670 | 98,591 | 98,592 | yes |
| 24 KiB ASCII | 24,942 | 24,863 | 24,864 | yes |
| 24 KiB controls | 147,822 | 147,743 | 147,744 | **no** |
| 64 KiB ASCII | 65,902 | 65,823 | 65,824 | yes |
| 64 KiB controls | 393,582 | 393,503 | 393,504 | **no** |
| 1 MiB ASCII | 1,048,942 | 1,048,863 | 1,048,864 | **no** |

The 24 KiB and larger records were deliberately inserted without admission;
current production send rejects those bodies. A written 24 KiB control record
caused both its exact journal read and general inbox read to fail with
`invalid state file`. Raising only the body guard can therefore corrupt the
usable inbox state even though the atomic write itself succeeds. Reply intents
embed a full outgoing message and use the same bounded reader, so recovery must
be included in a storage change.

JSON escaping can multiply body storage by six before metadata. History/sent
`body_omitted` saves response bytes **after** the journal record is decoded;
it does not reduce scan I/O or parsing. List queries still read every journal
record before ownership/filter checks, sorting, and limiting; exact-ID lookup
avoids that scan. At the existing 400-message/hour inbound count limit, increasing raw bodies from 16 to 64 KiB would raise the
raw maximum from 6.25 to 25 MiB/hour per target, before JSON and journal overhead.
Message history has no automatic body-retention window.

### Page omission is not body chunking

The normal page can replace an unusually large first body's content with
`body_omitted:true` and retain its ID. Exact lookup returns the full body.
No endpoint currently chunks a body. A larger default omission policy would
not solve exact-read or client context costs.

The following probe included the `data` envelope and the SDK-style mirrored
text/structured result. Full tool-result bytes exclude the outer JSON-RPC
response envelope:

| Body / view | Structured bytes | Full tool-result bytes |
| --- | ---: | ---: |
| 16 KiB ASCII exact | 16,636 | 33,362 |
| 16 KiB controls normal, omitted | 262 | 612 |
| 16 KiB controls exact | 98,556 | 213,586 |
| 64 KiB ASCII normal, omitted | 262 | 612 |
| 64 KiB ASCII exact | 65,788 | 131,666 |
| 64 KiB controls exact | 393,468 | 852,562 |
| 1 MiB ASCII exact | 1,048,828 | 2,097,746 |

These are encoded transport sizes, not model token counts. Which content
channel a client puts into context remains client-specific.

Zalki's 128 KiB MCP setting bounds **incoming** requests, not outgoing exact-read
responses. The pinned Go SDK transport's default is 16 MiB, but Zalki explicitly
selects 128 KiB for its server input. Raising that input limit would not establish
the receiving clients' independent response/context limits.

## Proposed adapter contract

After the core refactors merge, use an optional adapter interface rather than
adding another mandatory per-row discovery field:

```go
// Proposal only.
type MessageSizeAdapter interface {
    MaxBodyBytes(SessionView) int
}
```

Adapters without the extension keep the legacy 16 KiB allowance. A positive
adapter allowance is still bounded by a named global raw-body ceiling:

```text
effective_body_limit = min(global_max_body_bytes, target_max_body_bytes)
```

Limits count UTF-8 **bytes**, not JSON Schema string lengths or Go rune counts.
Keep UTF-8/NUL/capability checks shared. A valid raw body must also pass an
adapter-owned encoded-envelope/frame preflight. The latter must never silently
truncate or split an agent message.

Recommended staged ceilings:

| Target | Initial declared raw limit | Later candidate, only after prerequisites |
| --- | ---: | --- |
| Claude | 16 KiB plus existing encoded-frame check | 64 KiB, with a separately verified larger frame budget below the native character threshold |
| Codex | 16 KiB | 64 KiB only after native app-server **and** queue probes, including fallback |
| Cursor | 16 KiB | Keep 16 KiB until hook oversized-first-message handling and native hook-output limits are verified |
| External adapter without a declaration | 16 KiB | Explicit adapter opt-in, always bounded by global storage/framing policy |

A 64 KiB first global candidate is deliberately smaller than native Claude's
roughly million-character frame. It is a proposal, not verified capacity.
Do not enable it until escaped bodies, metadata, reply intents, exact reads,
and supported client routes pass the same boundaries end to end.

A future size error should state the actual target and effective byte limit,
without echoing content or credentials, for example:

```text
invalid_input: target cursor:UUID permits 16384 UTF-8 body bytes; received 20000
```

Encoded-frame failures should separately identify the serialized frame budget.
A size rejection must not consume flood quota, reserve a message, ACK a parent,
or leave an approval hold. Resolve the authenticated exact target and enforce
its limit before those effects, while preserving retry/idempotency behavior
for already committed IDs. Re-evaluate limits when a transport route can change;
an app-server-only size must not become an unsafe queue fallback allowance.

## Required implementation work, deferred

A safe increase needs more than the permitted constant/single-call edit:

1. Unify validation with the core owner's work, then add the resolved target
   check in reservation/reply admission. Today `validateMessageBody` runs before
   target resolution and `prepareMessage` repeats a fixed cap.
2. Bound encoded writes and corresponding message/reply readers consistently.
   Do not blindly increase the generic session/flood/state-file limit. Exercise
   crash recovery with a largest valid reply intent.
3. Align MCP/CLI input budgets and advertised body guidance with worst-case
   JSON expansion. Check receiving-client exact-response handling, and consider
   an explicit chunk/resource design before megabyte-scale bodies.
4. Preflight each adapter's complete encoded envelope. For Codex, include every
   permitted fallback; for Claude, keep native character and local byte budgets
   distinct and retain write-only receipt semantics.
5. For a future larger Cursor limit, emit an oversized-first-message reference
   stub fetchable by exact ID, or another explicit bounded delivery form,
   rather than leaving the queue blocked. Verify receipt/status semantics,
   generation fencing, and a later small message. A listener disappearing must
   not strand a message admitted for a larger listener-only allowance.
6. Measure scan/allocation/context costs with large bodies, including escaped
   JSON. Decide a byte budget in addition to message-count flood quotas if
   larger bodies are enabled.

## Validation and scope

All measurements used disposable journal fixtures, an offline extracted-handler
harness, or harmless local exec argument probes. No messages were posted to
native user sessions, and no model request was made. The temporary Go probe files
were removed after capturing results.

Existing frame-size, inbox/history/paging, and exact-contract tests were used to
check the source conclusions. The original investigation committed only this
design document and changed no adapter capacity or raw body limit.

The subsequent upgrade-robustness fix closes the journal writer gap: one shared
128 KiB limit applies to reads and the exact indented JSON plus newline written
to disk. An oversized merge is refused without dropping extensions or replacing
the old record. Receipt and ACK write failures return the prior durable state.
The 16 KiB message-body limit and native transport limits remain unchanged.
