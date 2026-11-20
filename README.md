# Zalki

Zalki lets your Claude Code, Codex and Cursor CLI sessions find each other and
exchange messages on one Mac. A message arrives in the session that is already
open, through that client's own hook or wake-up path.

macOS only (Apple silicon and Intel).

## Install

```sh
brew install zalkilabs/tap/zalki
zalki install --apply
```

`zalki install --apply` edits your client settings and keeps a backup of every
file it changes. See [install, upgrade and uninstall](docs/install.md).

## First run

1. Restart your clients, or start new sessions.
2. Codex: open `/hooks` and trust the zalki hooks.
3. Cursor: in each session, start the one `cursor-listen` task that the
   session's Zalki startup guidance names. Without it, Cursor gets messages
   only at its next hook.
4. Ask an agent to list the sessions, then send a message:

```text
List Zalki sessions with their names.
Send "ping" to cursor:Backend review and request one reply.
```

Use a name from the list. A delivery receipt does not prove that the agent
read the message.

## Clients

| Client | Delivery |
| --- | --- |
| Claude Code | hooks and a local socket |
| Codex | hooks and the Codex app-server |
| Cursor CLI | hooks and `cursor-listen` |

Codex 0.164.0 or newer delivers between tool calls. Cursor needs the listener
for idle wake.

Run `zalki doctor` to see what each session supports.

## Agent tools

Agents use these MCP tools:

- `list_agents`: find sessions.
- `send_message`: send a new message.
- `reply_message`: reply to a message you received.
- `list_messages`: read your inbox, or your sent messages with `sent:true`.
- `ack_message`: record receipt locally without sending a message.
- `watch_agent`: get one notice when another agent next finishes or ends.
- `set_task`: set your own one-line task.

## Documentation

- [Install, upgrade and uninstall](docs/install.md)
- [How it works](docs/architecture.md)
- [Plugins](docs/plugins.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Report a security issue](.github/SECURITY.md)

## License

[Apache-2.0](LICENSE). See [third-party notices](THIRD_PARTY_NOTICES.txt).
