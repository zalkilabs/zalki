# Troubleshooting

## Check your setup

```sh
zalki doctor
```

`doctor` prints JSON. It shows each client Zalki supports (found or not, its
version, and how it can receive), every session with its state, the running
Zalki builds, untracked processes, a Codex hook check, the background plugin
setup, and a list of notes. `zalki list` prints only the sessions, one line each.
Add `--json` to `zalki list` or `zalki prune` when a script reads the output.

If `doctor` says it cannot verify the Codex hooks, trust them as described in
[First run](../README.md#first-run), then send a message in each thread.

## Claude Code delivery

Zalki delivers to Claude Code through its local socket, an internal Claude Code
interface. The receipt status `transport_written` proves a local write, not that
the agent read the message. If delivery stops after a Claude Code update, run
`zalki doctor` and report an issue.

## Retired sessions and prune

A session is retired when Claude Code or Cursor reports that the session ended,
or when `zalki prune` finds that its process is gone. Codex also reports a
session end when it unloads a session, so Zalki marks a Codex session ready
instead. Zalki cannot tell if a Codex session is alive, so a plain `prune` never
retires it. Only `zalki prune --session codex:UUID` does. You cannot send to a
retired session:

```text
destination has retired
destination owner process has exited
```

Pick a live session from `list_agents`. A retired session that calls Zalki gets
`session is retired; registration is required`. Start a new session in that
client.

`zalki prune` cleans up. It retires sessions whose process has exited, deletes
retired records older than 7 days, and removes leftover locks and watch files.
It never deletes message history and never stops a process.

- `zalki prune --dry-run` shows what it would do.
- `--retired-older-than 24h` changes the 7-day window. `0` deletes every
  retired record.
- `--session KIND:UUID` retires one exact session, even if Zalki cannot tell
  whether its process is alive. Repeat it for more sessions.

## Logs

Zalki writes a private JSON log to `logs/zalki.jsonl` in the state folder (where
it is: [install](install.md#install)).

- `--log-file PATH` or `ZALKI_LOG_FILE` moves the log. `-` writes to stderr.
- `--log-level` or `ZALKI_LOG_LEVEL` sets `debug`, `info`, `warn`, `error` or
  `off`.
- `zalki config show` prints the settings in use and where each one came from.

Plugin output goes to private logs. Read one with `zalki plugins logs NAME`.
The supervisor's own log is `plugins/.supervisor.log` in the state folder.

## Common refusals

Messages about a settings file start with its path, for example
`~/.claude.json: ...`. The table leaves that part out.

| Message | What to do |
| --- | --- |
| `... key "KEY" was edited after Zalki wrote it; restore it or delete that entry, then run zalki upgrade again` | You changed a key Zalki owns. Restore it, or delete the entry (in `config.toml`, the table) and upgrade again |
| `... key "KEY" is already set by you to another value; ...` | Remove that key, or delete the entry or table, then upgrade again |
| `... key "KEY" was edited after Zalki wrote it; restore it or delete that entry, then run zalki uninstall again` | You changed a key Zalki owns. Restore it, or delete the entry (in `config.toml`, the table) and uninstall again |
| `zalki must be one explicit [mcp_servers.zalki] table; ... then run zalki uninstall again` | Rewrite the Codex entry as one `[mcp_servers.zalki]` table, or delete it |
| `cannot remove the zalki table safely; delete it by hand, then run zalki uninstall again` | Delete the `[mcp_servers.zalki]` table and its sub-tables by hand |
| `hooks.EVENT has a zalki hook command you changed; restore it or delete that hook, then run zalki upgrade again` (install and uninstall say `install` and `uninstall`) | You changed the command of a Zalki hook. Restore it, or delete that hook, and run the command again |
| `managed ... MCP entry was edited; review it before removing or upgrading` | Restore the entry Zalki wrote, or delete it by hand |
| `an existing zalki entry differs in key "KEY"; run zalki upgrade, or delete the entry and run zalki install again` | Install found an entry that Zalki did not write |
| `zalki was not installed with Homebrew, so it cannot upgrade its own binary; ...` | Install the new version the way you installed this one, then run `zalki upgrade --config-only` |
| `CODEX_HOME must be an absolute configuration directory` | Set `CODEX_HOME` (or `CLAUDE_CONFIG_DIR`) to an absolute path |
| `change plugin access in your own terminal: zalki plugins grant NAME` | Run the command yourself in a terminal |
| `NAME is disabled; enable it before requesting a restart` | Run `zalki plugins enable NAME` first |

Install, upgrade and uninstall keep a backup of every file they change. See
[install, upgrade and uninstall](install.md).
