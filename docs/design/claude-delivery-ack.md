# Claude socket delivery evidence and possible return-channel receipts

Status: current socket investigation and version diagnostics implemented;
return-channel listener proposed only. No new native receipt is implemented.

## Result

Claude Code 2.1.316 accepts Zalki's current user frame, but its inbound
connection has no positive acknowledgement or error response. Successful writes,
silence, and orderly connection close do not distinguish acceptance from a
silently ignored frame. Keep `transport_written` as evidence of a completed
local socket write. Do not downgrade it because an inline acknowledgement is
missing, and do not automatically resend.

A separate native return channel can report holds and their eventual outcomes.
It is not a general acceptance acknowledgement: immediate policy acceptance
does not send a status. Implementing a listener would improve visibility into
some holds, refusals, and drops, while leaving other successful writes uncertain.
A model read remains a separate fact, established by Zalki's explicit ACK/reply.

## Evidence and limits

The inspected macOS arm64 executable was
`~/.local/share/claude/versions/2.1.316`, reporting
`2.1.316 (Claude Code)`. Its SHA-256 was:

```text
64c327e92b2c5e0f78c0a980c5a518121e1f0d140ca771ca5dd4488d1a2dcb3f
```

The embedded readable inbox module occupies bytes `[280611271,280647118)`
of that binary. Its connection handler `en`, dispatchers `Qe`/`be`, and user
handler `ze` were inspected on 2026-11-02. Those are version-specific minified
symbols, not supported APIs. The module contains no socket `write` calls.
The connection handler ends the connection after peer EOF; it destroys it for
some authentication and size failures. Invalid JSON, unknown types, missing
content, and mismatched optional target session IDs can be ignored without any
response.

An offline harness evaluated the extracted connection handler with a mock
connection and a dispatch stub. It made no native session, network connection,
or model request. It observed:

| Input | Forwarded to dispatch | Response writes | Before sender EOF |
| --- | --- | --- | --- |
| Valid user frame | yes | 0 | open |
| Unknown message type | yes | 0 | open |
| User frame with missing content | yes | 0 | open |
| Invalid JSON | no | 0 | open |
| Required authentication absent | no | 0 | destroyed |
| Invalid first authentication frame | no | 0 | destroyed |

The dispatch stub means the first three rows do not prove policy admission.
Static inspection of the real dispatch/user handlers establishes the subsequent
type/content/session checks and peer-policy path. No frames were sent to live
user sessions. Do not treat these results as a protocol promise for other builds.

The public [inbox-socket documentation](https://code.claude.com/docs/en/cross-session-messaging#the-sessions-inbox-socket)
describes optional authentication on macOS/Linux and required authentication on
native Windows. It also says socket messages pass through inbound controls.
These controls can accept, hold, or refuse a peer message independently of a
completed local socket write. Zalki does not use a target's own-child token to
bypass peer policy.

## Recorded version verification

The adapter's embedded
[`socket-versions.json`](../../internal/adapters/claude/testdata/socket-versions.json)
owns exact-version evidence. A version number match is historical compatibility
coverage, not proof that every running session uses that executable or has the
same inbound settings. No version ranges are inferred.

| Version | Doctor classification | Coverage |
| --- | --- | --- |
| 2.1.307 | `fixture_covered` | Existing normalized hook/metadata fixture; no socket-admission claim |
| 2.1.316 on darwin/arm64 | `live_verified` | Four Claude smoke cases passed; current binary source reviewed |
| Other versions/platforms | `unverified_version` | Run a new disposable-session smoke check |

The existing private smoke report is
`~/.local/state/zalki/smoke-reports/f71f053d-3f74-4363-9a85-7a63c339533e.json`.
It started at `2026-11-01T15:44:38.037508000Z`, with Zalki build
`0.1.0-experimental+a984908f1cb0`. The report SHA-256 is:

```text
b64be950e7697c8ca3b628064090895e624fc7664379abfe7f09e42965181cbc
```

All four **Claude** observations record version 2.1.316 and passed:
fresh-registration, idle-wake, restart-recovery, and draft-preservation.
They contain correlated model replies of 238–278 bytes and
`transport_written` receipts. The report's aggregate `passed` field is false:
this is evidence for the four Claude cases, not a claim that the whole
multi-client run passed.

The historical smoke report records no Claude binary hash or explicit platform
field. darwin/arm64 is the observed host scope. The executable hash above belongs
to the later source review; it does not retroactively identify the smoke
executable. Future evidence should record both the version and executable hash
at smoke time. Keep raw reports private; commit only normalized provenance.

## Native return-channel shape

The existing frame has `msgV:1`, a UUID `msg_id`, `type:"user"`, a nonempty
string in `message.content`, and `priority:"next"`. The inspected user handler
processes it as peer-origin metadata with slash commands and attachment expansion
disabled. A sender that wants native status callbacks would additionally need
an address such as:

```json
{
  "msgV": 1,
  "msg_id": "00000000-0000-4000-8000-000000000099",
  "type": "user",
  "from": "uds:/private/tmp/cc-socks/12345.sock",
  "message": {"role": "user", "content": "scratch-only peer message"},
  "priority": "next"
}
```

The number in that illustrative path must be replaced by the real sending
process identity. It is not a path to an existing user's session. Optional
`session_id`, if present, identifies the target conversation; a mismatch is
silently rejected. Do not invent permission-mode metadata to change admission.

The native callback sender opens a **new** connection to `from`. Its frame has
`type:"control"`, `action:"peer_message_status"`, a newly generated control
`msg_id`, and `orig_msg_id` referring to the original user-frame UUID:

```json
{
  "msgV": 1,
  "msg_id": "00000000-0000-4000-8000-000000000100",
  "type": "control",
  "action": "peer_message_status",
  "orig_msg_id": "00000000-0000-4000-8000-000000000099",
  "status": "held",
  "from": "uds:/private/tmp/cc-socks/67890.sock"
}
```

Native status handling accepts these values:

| Status | Meaning in inspected code |
| --- | --- |
| `held` | Peer policy parked the message |
| `delivered` | A previously held message passed release/admission; not proof of model reading |
| `denied` | A held message was explicitly denied |
| `expired` | A hold expired, was cancelled, evicted, or settled on shutdown |
| `refused` | Peer policy refused it |
| `dropped` | Ingress guard rejected it; may include `drop_reason` and `dropped_msg_ids` |

The native writer encodes refusal as `status:"expired"` with
`status_detail:"refused"`; its receiver also accepts literal `refused`.
A future receiver must normalize both. Immediate acceptance has no matching
`delivered` emission in the inspected policy path. A timeout without a callback
therefore remains unknown, even when the message was accepted.

Return delivery is best effort. The inbox return-address guard requires a valid
`uds:` address in an allowed local socket namespace. The transport refuses
symlinks and can pin the return connection to the original sending PID and
process-start fingerprint. The original outbound sender and return listener
must therefore share the expected process identity; an arbitrary temporary path
or a separate helper cannot be assumed to work. These guards and native
cross-directory exceptions need a scratch proof before any implementation.

Native Claude also correlates status frames with its own outstanding-send map.
A Zalki listener would need its own bounded correlation state for
`orig_msg_id`, not Claude's in-memory map or the callback frame's new `msg_id`.

## Listener feasibility: proposal only

A short-lived listener owned by the **same MCP process** as the outbound write
could capture immediate hold/refusal/drop feedback, subject to the native path,
ownership, and peer-PID checks. The current MCP process stays alive between
calls, making it a more practical owner than a lifecycle-hook process.

A hook-side listener usually exits before a held message is approved, denied,
or expired. Detaching another process would change the PID the native sender
expects and introduce a new runtime/lifetime contract. A short response window
also misses later hold outcomes. Complete observation would need a listener
that lasts with the MCP runtime, bounded persisted correlation/recovery, and
a deliberate mapping into existing Zalki receipts.

Before building one, prove callback paths and sender identity in an isolated
session. Bind a private socket, start listening before sending, correlate only
known original IDs and targets, and validate the real peer process rather than
trusting JSON `from`. Preserve user-controlled inbound holds. Missing feedback
must not trigger a resend or turn an uncertain delivery into a safe failure.
No listener, native token storage, or receipt-state change is part of this branch.

## Verify a new version with disposable sessions

Normal tests use fixtures and do not invoke models. The manual smoke runner
sends real model requests and requires operator confirmations for startup,
idle, restart, and draft preservation.

1. Build the current branch with `make build`. Record `claude --version`, OS,
   architecture, and the SHA-256 of the resolved Claude executable **before**
   starting the test. Do not infer a range from one passing release.
2. Create separate temporary workspace, state, profile, and draft directories.
   Use a fresh Claude profile and transient configuration so normal user hooks,
   MCP servers, and project settings are not loaded. Authenticate and handle
   trust prompts through Claude's normal controls.
3. Generate configuration drafts for the scratch state; do not apply them to
   the user's normal profile. Launch a fresh disposable Claude using only these
   hooks and MCP configuration. An example from the repository root:

   ```sh
   smoke_root=$(mktemp -d /private/tmp/zalki-claude-smoke.XXXXXX)
   mkdir -p "$smoke_root/workspace" "$smoke_root/state" "$smoke_root/profile"
   ./bin/zalki setup --state "$smoke_root/state" --out "$smoke_root/drafts"
   cd "$smoke_root/workspace"
   CLAUDE_CONFIG_DIR="$smoke_root/profile" claude \
     --setting-sources "" \
     --settings "$smoke_root/drafts/claude-hooks.json" \
     --strict-mcp-config \
     --mcp-config "$smoke_root/drafts/claude-mcp.json"
   ```

   The isolation flags are described in the
   [Claude CLI reference](https://code.claude.com/docs/en/cli-reference). Managed
   policy settings still apply.

   Keep this terminal open. When the runner asks for a restart, restart/resume
   only this disposable conversation with the same scratch profile and flags.
   Do not use `--dangerously-skip-permissions` or change the user's inbound policy
   to make a test pass.
4. In another terminal, return to this repository and list only the scratch
   state with `./bin/zalki list --state /absolute/scratch/state`. Select the exact
   disposable Claude ID from that state, then run:

   ```sh
   export ZALKI_SMOKE_STATE=/absolute/scratch/state
   export ZALKI_SMOKE_WORKSPACE=/absolute/scratch/workspace
   export ZALKI_SMOKE_TARGETS='claude:EXACT_DISPOSABLE_UUID'
   make smoke-live
   ```

5. Confirm all four Claude scenarios, their recorded version, correlated model
   replies, and draft observation in the private report. A transport receipt
   alone is insufficient. Inspect per-client observations even if a multi-client
   report has an aggregate failure. Do not run a rejection/hold probe against
   live user sessions.
6. Add an exact-version/platform evidence entry with the report hash, all four
   results, Zalki build, and Claude executable hash captured at test time. Review
   new binary parser behavior separately. Add a normalized fixture only for
   shapes actually observed, update version-table tests, and rerun the full
   checks plus `make cost-check`.

The recipe is opt-in and was not executed during this investigation. This branch
used read-only binary/report inspection, the offline handler harness, and
isolated fake-client/socket tests.
