# Connection identity binding proposal

Keep explicit actor/capability authentication until a client proves that its
MCP connection belongs to exactly one conversation. Current Zalki hooks deliver
credentials to model context; the shared installation configuration does not
inject them into an MCP process. Cursor also returns session `env`, but that
does not prove those values reach an already running MCP server.

- Claude: [stdio server lifetime](https://code.claude.com/docs/en/mcp#how-claude-code-stops-a-stdio-server)
  follows the session, and MCP configuration supports environment expansion.
  However, [SessionStart environment persistence](https://code.claude.com/docs/en/hooks#persist-environment-variables)
  documents later shell commands, not retroactive MCP environment updates.
- Codex: [official OpenAI documentation](https://learn.chatgpt.com/docs/extend/mcp?surface=cli)
  supports `env` and `env_vars` for stdio. It does not establish one MCP process
  per thread across CLI, desktop, and app-server modes. An app-server parent
  process or a shared config file is not conversation identity.
- Cursor: [CLI MCP](https://cursor.com/docs/cli/mcp) shares the editor's
  configuration. [SessionStart env](https://cursor.com/docs/hooks#sessionstart)
  is documented for subsequent hooks, not a per-conversation MCP restart or
  credential channel. Process sharing across conversations remains unproven.

A read-only local snapshot on 2026-11-01 found 18 running Zalki MCP processes:
12 with Codex parents, five with Claude parents, and one with a Cursor CLI
parent. Each had zero or one successfully observed actor. This is an
observation, not an isolation guarantee; no client models were started and no
credentials or environment values were read for the process check.

Propose an opt-in, dedicated per-conversation launcher which receives its own
actor/capability through a trusted private channel before MCP starts. Bind once,
reject mismatching identities, and authenticate against the store on every
call. Missing credentials, expired registration, or capability rotation must
fail closed and require a trusted refresh/reconnect. Never infer identity from
a name, cwd, parent daemon, another connection, or the first model-supplied actor;
never put per-session capabilities in shared client config or logs. Shared
connections retain explicit authentication. Before implementing, probe two
disposable conversations per client (including Codex app-server and Cursor
editor), then resume, compact, and reconnect each independently to verify
connection ownership and credential refresh without cross-session reuse.
