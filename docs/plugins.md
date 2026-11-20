# Plugins

A plugin is a program that you install from a local folder. It can read Zalki
events and messages, and answer or send messages. Zalki ships no plugins.

**A plugin is not sandboxed.** It runs as you. It can read and change all of
your Zalki state and all files you can access. Access settings limit only what
Zalki gives it. Install only code you trust.

## Install

```sh
zalki plugins install ./my-plugin
```

Zalki copies the folder to a private place and shows one review screen with one
`[y/N]` question. After yes, it builds the copy, pins the result, records the
access and enables the plugin.

- `--as NAME` installs one more instance with its own data and access.
- `--details` shows source, build and trust details first.
- `--dry-run` builds and checks the folder without touching Zalki state.
- `--yes` skips the screen. It refuses a manifest that asks for send or read
  access. Without a terminal it also needs `--tree-sha256` and
  `--allow-unsandboxed`.

## Manage plugins

| Command | What it does |
| --- | --- |
| `zalki plugins list` | List plugins with version, state, scope and access |
| `zalki plugins show NAME` | Show install facts and access (`--details` for hashes) |
| `zalki plugins status NAME` | Print state, access and run status as JSON |
| `zalki plugins enable NAME`, `disable NAME` | Turn a plugin on or off |
| `zalki plugins configure NAME --scope all` | Apply standard access to every live session (`none`: to no session) |
| `zalki plugins update NAME [FOLDER]` | Review and replace a plugin; keeps its identity, data, scope and enabled state |
| `zalki plugins remove NAME` | Remove a plugin (it is disabled first) |
| `zalki plugins check NAME` | Test the plugin's transport (also `--exec /abs/path -- ARGS`); a temporary HOME and cwd do not sandbox it |
| `zalki plugins add NAME --exec /abs/path -- ARGS` | Add a built program by hand (disabled) |

Stop a running plugin before you update it. New access in an update starts
unselected; review it in your own terminal. `update NAME --check` exits 0 for
up to date, 2 for an update, and 1 for an error.

## Access

| Kind | Meaning |
| --- | --- |
| `--status` | See state and message status of sessions in scope, with no text |
| `--receive` | Sessions in scope may message `plugin:UUID`. The plugin may answer each message (at most 8 answers, within 24 hours) |
| `--send SESSION` | Send messages to one exact session, unasked, as the plugin (repeat) |
| `--read SESSION` | Read received messages of one exact session, from any sender (repeat) |

`status` and `receive` are standard access: they apply to every live session
when the scope is `all`. `send` and `read` are extended access, for exact
sessions only. What you do not grant is denied.

Set access with `zalki plugins grant NAME --status --receive`. `grant` replaces
all earlier access. `zalki plugins revoke NAME` removes all access and disables
the plugin. Run `grant` and `configure --scope all` in your own terminal.
A plugin installed from a folder can only get the kinds its manifest asks for.

To read one message without starting the plugin or acknowledging it, use
`zalki plugins read NAME --session SESSION --message UUID` or
`zalki plugins inbox-read NAME --message UUID`.

A session can opt out. An agent can run `zalki plugins mute NAME` and
`unmute NAME` for its own session. When a session starts, `ZALKI_PLUGINS=off`
gives it no plugins, and a comma-separated list of names allows only those.

## Run a plugin

Enabled does not mean running. `zalki plugins run NAME` runs one plugin in the
foreground. `zalki plugins supervise` runs all enabled plugins and restarts them
after a failure. After 5 failures in 10 minutes a plugin stays stopped until you
run `zalki plugins restart NAME`. If you have enabled plugins,
`zalki install --apply` can set up one macOS login item that runs the
supervisor. It asks first. Turn it off in System Settings > General > Login
Items & Extensions.

The first time a background plugin starts, Zalki queues one macOS notification.
`zalki plugins logs NAME` shows the last 50 lines of the plugin's private log,
which may hold secrets the plugin prints.

## Write a plugin

A plugin folder holds `zalki-plugin.json` (up to 16 KiB) and the source:

```json
{
  "api": "zalki-plugin/1",
  "id": "local/my-plugin",
  "version": "0.1.0",
  "name": "my-plugin",
  "language": "go",
  "purpose": "Shown on the review screen",
  "build": ["go", "build", "-trimpath", "-o", "my-plugin", "."],
  "artifact": "my-plugin",
  "access": {"status": true, "receive": false, "send": false, "read": false}
}
```

- `id` is `author/name`: a claim by the author, not a check.
- `language` is `go` or `native`. `native` needs `"build": null`. A `go` build
  may use only `-trimpath`, `-mod=readonly`, `-p=1`, `-p=2`, `-ldflags "-s -w"`,
  one `-o` and local packages.
- `artifact` is the program, a path inside the folder. `args` is optional.
  `access` must ask for at least one kind.

The plugin speaks JSON lines (one frame per line, up to 64 KiB) on stdin and
stdout. The host sends its hello first, the plugin answers with its own hello,
and the host sends `hello_ok`. Then the host sends events, and the plugin
acknowledges each one. The host offers `feed` (status), `inbox` and `reply`
(receive), `send` and `read`, as granted. Example frames are in
`internal/plugins/testdata/plugin-wire/frames.jsonl`.

The process must exit within 2 seconds when stdin closes, and must not
daemonize. Helpers must exit with it and must not keep file descriptor 3. It
gets only `HOME` and `PATH` in its environment. Its working directory is its
own data folder. Terminal input does not work.
