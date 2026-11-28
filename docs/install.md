# Install, upgrade and uninstall

## Install

```sh
brew install zalkilabs/tap/zalki
zalki install
```

`zalki install` lists the files it will change and asks one question
(`[y/N]`). On `y` it edits your client settings and prints `Updated:` for each
file it changed. On anything else it prints `Nothing changed.` Then restart
your clients and follow [First run](../README.md#first-run).

- `zalki install --yes` skips the question. Without a terminal, install needs
  `--yes`.
- `zalki install --dry-run` prints a JSON preview and changes nothing.
- `zalki setup --out DIR` writes drafts of the settings to an empty folder `DIR`
  for review. `zalki setup` without `--out` prints the preview.

With `CODEX_HOME` or `CLAUDE_CONFIG_DIR`, set an absolute path before you
install; the files then live there. Cursor files stay under `~/.cursor`.

By default Zalki keeps its state in `$XDG_STATE_HOME/zalki` (when that is an
absolute path) or `~/.local/state/zalki`, and its settings in
`$XDG_CONFIG_HOME/zalki/config.json` or `~/.config/zalki/config.json`. `--state`
or `ZALKI_STATE`, and `--config` or `ZALKI_CONFIG`, change them. It records what
it wrote in `installation.json` in the state folder.

## What it changes

| Client | File | Change |
| --- | --- | --- |
| Claude Code | `~/.claude/settings.json` | hooks for `SessionStart`, `SessionEnd`, `PermissionRequest`, `PostToolUse`, `UserPromptSubmit`, `Stop` |
| Claude Code | `~/.claude.json` | `mcpServers.zalki` with `command`, `args`, `type` |
| Codex | `~/.codex/hooks.json` | the same hooks, plus `Interrupt` |
| Codex | `~/.codex/config.toml` | table `[mcp_servers.zalki]` with `command`, `args` |
| Cursor | `~/.cursor/hooks.json` | hooks for `sessionStart`, `sessionEnd`, `beforeSubmitPrompt`, `postToolUse`, `stop` |
| Cursor | `~/.cursor/mcp.json` | `mcpServers.zalki` with `command`, `args` |

Your other settings and hooks stay as they are.

## Backups

Before Zalki changes an existing file, it copies the file to
`<file>.zalki-backup-<random>` and never deletes that copy. If a write fails,
Zalki restores the files it already changed.

## Upgrade

```sh
zalki upgrade
```

This runs `brew upgrade --cask zalkilabs/tap/zalki`, then the new build
refreshes the settings above. It prints `Upgrade successful` when done.
Restart your sessions afterwards so they use the new build.

- `zalki upgrade --dry-run` prints a JSON preview and changes nothing.
- If you ran `brew upgrade` yourself, run `zalki upgrade --config-only`.
- Without Homebrew, `zalki upgrade` fails: install the new binary yourself, then
  run `zalki upgrade --config-only`.

### Your own keys and hooks

You may add keys to the Zalki entry, for example `env` in `mcpServers.zalki`.
Upgrade keeps them. It rewrites only the keys Zalki owns:

- Claude Code: `command`, `args`, `type`
- Codex and Cursor: `command`, `args`

In `config.toml`, Zalki replaces only those values. Comments and order stay.
Keep exactly one `[mcp_servers.zalki]` table.

Upgrade refuses, and names the key, in two cases:

- You edited or removed an owned key. Restore it, or delete the entry, then
  run `zalki upgrade` again.
- You set a key that Zalki now owns to another value. Remove it, or delete the
  entry, then run `zalki upgrade` again.

Upgrade also keeps a Zalki hook that you edited and updates only its
`command`, so no second Zalki hook appears. If you changed the command itself
(a wrapper, a redirect or an environment prefix), upgrade stops and names the
event. Restore the command or delete that hook, then run `zalki upgrade` again.
An old extra Zalki hook from an earlier upgrade can cause this stop once.

## Uninstall

```sh
zalki uninstall
```

Uninstall lists the files it will change and asks one question. On `y`, Zalki
removes the hooks and MCP entries it wrote, the `installation.json` record, and
the Zalki login item if one is installed. Hooks you added stay. Message history
stays. `--yes` skips the question (needed without a terminal); `--dry-run`
prints a JSON preview and changes nothing.

If you added keys to the Zalki entry (for example `env`), or added or changed
keys on a Zalki hook, uninstall removes the whole entry or hook with them,
sub-tables included. The preview and the result list the key names, never the
values. The backup of that file keeps the old content, so it may hold secrets.
It is private (mode 0600) and Zalki never deletes it. You may want to delete it
yourself.

If you edited a key that Zalki owns, or the command of a Zalki hook, uninstall
refuses. It names the key (or the event, for a hook). Restore it, or delete the
entry or hook, then run `zalki uninstall` again. See
[troubleshooting](troubleshooting.md#common-refusals).

## Codex tool approval

Zalki never changes approval settings, so Codex may review each Zalki call.
To stop that, add this under `[mcp_servers.zalki]` in `~/.codex/config.toml`
(Codex 0.122 or newer):

```toml
default_tools_approval_mode = "approve"
```

This approves all Zalki tools, including send and reply. Upgrade keeps the
line. Uninstall removes it with the table and lists it. Add it again after you
reinstall.
