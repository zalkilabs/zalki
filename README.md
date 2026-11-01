# Zalki

Local messaging between Claude Code, Codex, and Cursor CLI sessions.
A message shows up inside the session that is already open, through that
client's own hook, socket, or idle wake, with a private capability and a
receipt.

Discover agents by name, send messages, and reply from their normal CLI.

macOS preview. Claude's transport is experimental. Cursor idle wake
requires one native Shell background task, armed from its startup guidance.

## Install

Homebrew installs a signed, notarized macOS binary;
Go is required only for development builds.

```sh
brew install zalkilabs/tap/zalki
zalki install --apply
```

Installation preserves unrelated settings and backs up changed files. Restart
your clients and trust the integrations through their normal controls.
Use `install` without `--apply` to generate configuration drafts for review.
A fresh Codex session becomes discoverable after its first prompt.

If you use `CODEX_HOME` or `CLAUDE_CONFIG_DIR`, export them before installing.
Cursor hooks and MCP settings remain under `~/.cursor`.

## Use

Ask an agent:

```text
List Zalki sessions with their names.
Send "ping" to cursor:Backend review and request one reply.
```

Use a name from the list. Replies arrive through the client's native delivery path.
Transport acceptance does not confirm that the receiving agent read the message.

`send_message` needs `actor`, `capability`, `to`, and `body`. It generates an ID
when `message_id` is omitted and returns that ID in the receipt. For safe retries,
provide a stable `message_id` before the first attempt and reuse it with the same
destination and body. If a call without an ID times out after writing, retrying
without an ID can send a duplicate; identical text can also be an intentional
new message, so Zalki does not deduplicate it by body.

`reply_message` needs `actor`, `capability`, the **incoming** `message_id`, and
`body`. It already generates the outgoing ID and safely reuses it for the same
parent and reply body. Receipts include a short `note` when delivery is deferred
or approval prevents sending; the status remains authoritative. A busy-target
note describes timing observed at that attempt, not proof of reading.

Use `list_messages` with `sent:true` to see your sent messages and their delivery
status and `acknowledged_at`, newest first. `history:true` includes both sent and
received history. These views omit bodies unless `include_body:true`; continue
pages with the same options and `cursor=next_cursor`. Default reads remain the
unread inbox. `list_agents` takes no arguments.

Agent messages have flood limits: each sender-to-target pair has a burst of 12,
refills 12 messages/minute, and can admit 120 messages in a rolling hour. A
target's combined limit is a burst of 48, refill of 40/minute, and 400/hour.
Over-limit calls return `rate_limited` with a retry time; they are never silently
queued. Reuse the same message ID for a deliberate retry. Completion notices
are exempt. See the [design and traffic measurements](docs/design/flood-protection.md)
for storage behavior and the proposed human-reset loop guard.

`watch_agent` asks for one notice when another agent next finishes a turn or
ends its session. The notice is a Zalki notice, not a message from that agent,
and a reply to it is rejected. It can include up to 200 bytes of that agent's
own last line. The agent does not opt in; Claude Code's `notify_when_idle`
forwards a status line the same way. Codex has no session-end hook, so a
Codex watch fires only when a turn goes idle.

The MCP `list_agents` tool returns each agent's ID, optional name, optional
`task`, working directory, and `updated_at` (last recorded registration or
lifecycle activity, in UTC seconds, not a heartbeat). `task` is that agent's
own unverified claim about its current work. It is omitted when empty and is
not an address. `set_task` is the only way to set it. The ID prefix identifies
the kind. Omitted `state` means `ready`, omitted `liveness` means `unknown`, and
omitted `experimental` means false. `delivery` is the busy-session timing:
`between_tools`, `next_turn`, `native_notification`, or `next_hook`. All except
`next_hook` imply idle wake; an explicit `idle_wake` overrides that default.
These are per-session capabilities: Cursor's listener and Codex's verified
app-server support can change them. They do not prove that a terminal is live
or a message was read. Agents in `approval` refuse delivery until approval ends.
For full transport and setup details, use `zalki list` or `zalki doctor`;
the CLI JSON format is unchanged.

```sh
zalki doctor
zalki upgrade --apply
zalki uninstall --apply
```

Upgrade and uninstall preview changes when `--apply` is omitted.
Use `--state` to choose the state directory.

### Upgrading

Every Zalki process that shares a state directory must run the same build.
Older builds cannot read the records a newer listener writes, and one such
record stops their session listing, discovery and `doctor` for every client,
not just Cursor. After installing a new binary:

1. Run `zalki upgrade --apply`.
2. Reconnect the Zalki MCP server in every open Claude, Codex and Cursor
   session (or restart those sessions).
3. In each Cursor conversation, cancel any running `cursor-listen` task, then
   arm one new task from its startup guidance.

Message history and pending messages are kept. Do not delete the state
directory to recover from a mixed-build error; finish the steps above instead.

## Development

Use Go 1.26.10 or newer.

```sh
make build
make test vet
make completions
make release-snapshot
```

Release builds require GoReleaser. Tests use isolated fixtures and include race
checks and MCP stdio integration; they do not call live agent models.
See [contributing](.github/CONTRIBUTING.md) and [security reporting](.github/SECURITY.md).

## License

[Apache-2.0](LICENSE). See [third-party notices](THIRD_PARTY_NOTICES.txt).
