# Zalki

**macOS preview.** Claude Code’s native transport is experimental.

Zalki lets Claude Code, Codex, and Cursor CLI sessions message each other on one computer.
Ask another agent for a review or share a result without copying text between terminals.
For developers running several coding agents under the same OS account.

```text
Codex → Claude: Please review the parser change.
Claude → Codex: The empty-input case needs a check.
```

## Quickstart

Install the macOS preview with Homebrew and draft the client settings.

```sh
brew install zalkilabs/tap/zalki
zalki install
```

Review the drafts in `generated/`, then apply the settings. Zalki backs up changed files.

```sh
zalki install --apply
```

`zalki setup` also writes configuration drafts.
Export `CODEX_HOME` or `CLAUDE_CONFIG_DIR` first if you use custom locations.
The installer writes Cursor settings under `~/.cursor`.

1. Start or restart two supported CLI clients.
2. Review and trust Zalki in each client.
3. Send a first prompt in each client.

In Codex, try asking for a review.

```text
List Zalki sessions. Send "Please review my change" to the Claude session
and ask for one reply.
```

Use the session ID or unique name from the list. Run `zalki doctor` if setup or delivery fails.

## How it works

Hooks register sessions and update their state.
Agents use MCP tools to discover peers, send messages, and reply.
Client adapters use local sockets, native queues, or hooks for delivery.
State and message history live in `~/.local/state/zalki`. Use `--state` to choose another directory.
For safe retries, supply a stable `message_id` before the first send and reuse it with the same destination and body.
A transport receipt does not prove the receiving agent read the message.

**MCP tools** `list_agents`, `send_message`, `list_messages`, `reply_message`, `ack_message`, `set_task`, `watch_agent`.

## Supported clients

| Client | Delivery | Limits |
| --- | --- | --- |
| Claude Code | Local inbox socket with delivery between tool calls and idle wake. | Experimental native interface. `transport_written` confirms only a local write. |
| Codex | Verified app-server delivery when available, with a native queue fallback. | Support depends on the version and profile. Busy queue delivery may wait for the next turn. |
| Cursor CLI | Hooks with idle wake through a native Shell background task. | Follow startup guidance to arm `cursor-listen`. Otherwise delivery waits for a tool-use or completed-stop hook. |

Messages can wait while a target has a permission prompt open.
See the [Claude delivery evidence](docs/design/claude-delivery-ack.md),
[traffic limits](docs/design/flood-protection.md), and [watch lifecycle](docs/design/watch-lifecycle.md).

## Security and trust

Zalki routes messages locally, with no cloud relay or TCP listener.
Your clients may include received text in requests to their model provider.
State uses owner-only directories and files. Treat processes running under your OS account as trusted.
See the [security policy](.github/SECURITY.md) for reporting issues.

## Plugins on `main`

Builds from `main` include experimental, opt-in plugins. Use `zalki plugins --help` for the current commands.
Register a trusted executable, review its grants, then enable it explicitly.
Plugins run with your OS permissions and can read or change your Zalki state. Grants filter Zalki data. File and network access follow OS permissions.

## Update or remove integrations

The Homebrew preview `v0.1.0-rc.6` uses two commands to update the binary and refresh client settings.

```sh
brew upgrade --cask zalkilabs/tap/zalki
zalki upgrade --apply
```

For newer Homebrew-managed builds from `main`, `zalki upgrade` handles both steps. Follow the command’s reconnect guidance.
Use the same build for processes sharing a state directory. See [shared-state upgrades](.github/CONTRIBUTING.md#shared-state-upgrades).
Preview removal with `zalki uninstall`, then apply it with `zalki uninstall --apply`.

## Development

Use Go 1.26.10 or newer.

```sh
make build
make test vet lint
```

Default tests use isolated fixtures and race checks, without calling live models.
`make test-required` also checks required integration coverage.
See [contributing](.github/CONTRIBUTING.md) for formatting, packaging, and release targets.

For the earlier tool, pruning, and recovery reference, see the [previous README](https://github.com/zalkilabs/zalki/blob/66e03290d26eeaa9bf50b63c2284e39c5e4cf2af/README.md).

## License

[Apache-2.0](LICENSE). See [third-party notices](THIRD_PARTY_NOTICES.txt).
