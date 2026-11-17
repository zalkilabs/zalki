# Moving from rc.7-era state to rc.8

rc.8 requires current registration and runtime records. rc.7-era records do not
supply those facts, and there is no compatibility reader or automatic migration.
Do not treat a resumed old chat as a new registration: its native chat ID stays
the same. Start NEW chats with NEW native chat IDs after the transition.

This is an offline clean install, using an archive-capable bridge build **before
normal rc.8 setup or use**. The installed rc.7 binary cannot run a command it does
not contain. `zalki upgrade` updates the binary/configuration and prints this
handoff; it does not supply the bridge, archive state, migrate messages, or certify
that the offline transition has already happened.

1. Retain the old binary and make separate backups of external client
   configuration. Close all old chats and MCP/listener processes. Stop background
   plugins, the supervisor and automatic restarts. Keep them stopped until the
   move is confirmed; the archive command refuses observed activity but is not an
   all-writer fence for sessions started during the move.
2. Stage and verify the separately supplied archive-capable bridge executable
   without running normal install/setup/upgrade, refreshing integrations, or
   starting clients/services. Use that executable with the same state selector as
   the old installation. For an explicit state path, the command is:

   ```sh
   "/path/to/archive-capable-zalki" --state "/absolute/path/to/old-state" state archive
   ```

   If no verified archive-capable build is supplied, this prerequisite is missing;
   do not substitute a normal rc.7 upgrade or a compatibility reader.
3. Check the exact source and destination shown by the archive command. Its
   existing terminal confirmation authorizes a **MOVE**, never deletion: old
   Zalki history, registrations, plugin data and settings inside the selected
   state are retained in the archive. Decline or EOF leaves state unmoved. A move
   is not a second backup copy. If a move or its durability is unconfirmed, inspect
   both named paths before any retry or fresh setup; do not repeat it blindly.
4. After a confirmed archive, install rc.8 and refresh client integration on fresh
   state. Reinstall plugins and review their access instead of assuming old plugin
   registrations/permissions were restored. Follow the installer's existing
   ownership/conflict checks: the archive does not reset external client settings.
   Then start NEW chats with NEW native
   chat IDs. Do not resume rc.7 chats for Zalki messaging; reconnecting an old chat
   does not create a new ID. Old Zalki history/messages are not replayed into the
   new state. Native chat transcripts outside Zalki state are not deleted.

Keep the archive, old binary and external configuration backups. There is no
implemented restore/merge procedure here. A future rollback must first stop all
participants again and preserve any new rc.8 data separately; do not overwrite or
merge it, and do not assume the older binary can read rc.8 state.
