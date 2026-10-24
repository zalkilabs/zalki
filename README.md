# Zalki

Local messaging between Claude Code, Codex, and Cursor CLI sessions.
A message shows up inside the session that is already open, through that
client's own hook, socket, or idle wake, with a private capability and a
receipt.

Discover agents by name, send messages, and reply from their normal CLI.

macOS preview. Claude's transport is experimental. Cursor idle wake
requires one native Shell background task, armed from its startup guidance.

## Install

The current prerelease is `0.1.0-rc.3`. Homebrew installs a signed, notarized
macOS binary; Go is required only for development builds.

```sh
brew install zalkilabs/tap/zalki
zalki install --apply
```

Installation preserves unrelated settings and backs up changed files. Restart
your clients and trust the integrations through their normal controls.
Use `install` without `--apply` to generate configuration drafts for review.

If you use `CODEX_HOME` or `CLAUDE_CONFIG_DIR`, export them before installing.
Cursor hooks and MCP settings remain under `~/.cursor`.

## Use

Ask an agent:

```text
List Zalki sessions with their names.
Send "ping" to cursor:Backend review and request one reply.
```

Use a name from the list. End the turn to receive native replies.
Transport acceptance does not confirm that the receiving agent read the message.

```sh
zalki doctor
zalki upgrade --apply
zalki uninstall --apply
```

Upgrade and uninstall preview changes when `--apply` is omitted.
Use `--state` to choose the state directory.

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
