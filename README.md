# Zalki

Local messaging between Claude Code, Codex, and Cursor CLI sessions.
A message shows up inside the session that is already open, through that
client's own hook, socket, or idle wake, with a private capability and a
receipt.

Discover agents by name, send messages, and reply from their normal CLI.

macOS preview. Claude's transport is experimental. Cursor idle wake
requires one native Shell background task, armed from its startup guidance.
Claude socket writes have no native inline ACK; `transport_written` confirms
only the local write. See the [version evidence and smoke recipe](docs/design/claude-delivery-ack.md).

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
or queued for approval; the status remains authoritative. A busy-target
note describes timing observed at that attempt, not proof of reading.

Both tools also accept `message` for `body`; `reply_message` accepts
`reply_to` for the incoming `message_id`. The advertised schemas keep the
canonical names. If both spellings are supplied, their string values must match;
a conflict fails before sending or acknowledging anything.

Use `list_messages` with `sent:true` to see your sent messages and their delivery
status and `acknowledged_at`, newest first. `history:true` includes both sent and
received history. These views omit bodies unless `include_body:true`; continue
pages with the same options and `cursor=next_cursor`. Default reads remain the
unread inbox. With `sent:true`, optionally filter by `to` (an exact recipient
ID or current unique name) and request `preview:true` for a single-line preview
of at most 80 UTF-8 bytes. Previews omit the rest of the body; full-body access
still follows the existing rules. Use the exact ID to filter a deleted recipient.
Neither option is accepted on inbox reads.

When a target is waiting on a permission prompt (`approval`), Zalki holds up to
eight messages for that session without injecting or acknowledging them. Delivery
resumes through the normal lifecycle hooks after the prompt is answered. A full
approval queue rejects additional sends; a queued message's receipt does not
confirm that the target read it. Retirement, owner exit, or a changed registration
can settle queued messages as not sent; check their receipts.

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
Codex watch fires on a normal exit only when a turn goes idle.

If a watched agent is killed or crashes, an ended notice still arrives with no
command needed: each running `zalki mcp` server watches the owner processes of
watched agents (kqueue on macOS, pidfd and inotify on Linux), so the notice is
about immediate for process-owned sessions and takes up to five minutes for
sessions whose liveness is endpoint-based. It needs at least one running MCP
server on a build with this feature; servers on older builds do not observe.
See [watch lifecycle](docs/design/watch-lifecycle.md).

The MCP `list_agents` tool works without arguments. Optionally provide your
`actor` and `capability` together to mark your row with `self:true`.
Incomplete or invalid credentials return an error. It returns each agent's ID,
optional name, optional `task`, working directory, and `updated_at` (last recorded
registration or lifecycle activity, in UTC seconds, not a heartbeat). `task` is that agent's
own unverified claim about its current work. It is omitted when empty and is
not an address. `set_task` is the only way to set it. The ID prefix identifies
the kind. Omitted `state` means `ready`, omitted `liveness` means `unknown`, and
omitted `experimental` means false. `delivery` is the busy-session timing:
`between_tools`, `next_turn`, `native_notification`, or `next_hook`. All except
`next_hook` imply idle wake; an explicit `idle_wake` overrides that default.
These are per-session capabilities: Cursor's listener and Codex's verified
app-server support can change them. They do not prove that a terminal is live
or a message was read. Permission-prompt queues follow the limits described above.
For full transport and setup details, use `zalki list` or `zalki doctor`;
the CLI JSON format is unchanged.

```sh
zalki doctor
zalki upgrade --apply
zalki uninstall --apply
```

Upgrade and uninstall preview changes when `--apply` is omitted.
Use `--state` to choose the state directory.

### Retired sessions

`zalki prune --dry-run` previews cleanup; `zalki prune` applies it. Prune retires
sessions with confirmed owner exit and removes already-retired session records
older than seven days by default, while keeping message history. Age alone does
not retire an active registration. Use `--retired-older-than` to change the
retention window (for example, `336h` for fourteen days); `0` removes every
already-retired record. Cleanup runs only when you invoke prune. For a legacy
registration whose owner cannot be checked, `--session kind:UUID` explicitly
selects that exact session for retirement.

Prune also removes completion-watch records that can no longer fire: expired
watches, unreadable or invalid watch files, and watches whose target session
record is gone or is deleted by retention in the same run, plus any per-target
watch directory left empty. They are listed in `deleted_watches`; a dry run
lists exactly what an applied run deletes. Watches on live sessions that have
not expired are kept. Symlinks and unrecognized names are never followed or
removed.

### Upgrading

Use the same build for every Zalki process sharing a state directory. This
build skips unreadable or unsupported sessions in lists and discovery, reports
a skipped count, and keeps exact access strict. Older releases can still fail
entire scans when they encounter a newer record. After installing a new binary:

1. Run `zalki upgrade --apply`.
2. Reconnect the Zalki MCP server in every open Claude, Codex and Cursor
   session (or restart those sessions).
3. In each Cursor conversation, cancel any running `cursor-listen` task, then
   arm one new task from its startup guidance.

`state-formats.json` is created lazily; rc.5 ignores it. An unreadable or
unsupported marker fences writes with an "upgrade zalki" error while known data
remains readable. For an unreadable marker, restore it from backup, or remove
it only after confirming every running Zalki build is this version or newer.
Do not remove a valid marker to bypass an unsupported format.

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
