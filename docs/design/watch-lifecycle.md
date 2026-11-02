# Completion watches and exit observation

A completion watch fires once when a target completes a turn or ends its
session through a lifecycle hook. Notices copy at most 200 UTF-8 bytes of its
last status line, ending at a word boundary when possible, with an ellipsis when truncated.
The status text is the target's unverified claim, not user approval.

A subscription created after a Stop handler has started remains armed for the
next completed Stop, so an already-running completion cannot consume a newer
watch. Expired watches are removed without a notice; watches last twelve hours.

## Exit observation

A kill or crash runs no SessionEnd hook, so a background observer notices it.
Every `zalki mcp` server (one per client session) runs `Service.ObserveExits` in
its own goroutine; no daemon or launchd job is involved. Because a watcher arms
its watch through its own MCP server, at least one observer is alive whenever a
live watcher exists. Servers that are still on an older build do not observe.

The observer is event-driven:

- **Process exit.** For each watched target it registers the session's recorded
  owner process with the operating system and is told when it exits: kqueue
  `EVFILT_PROC` on macOS, pidfd on Linux. The recorded start time is verified
  after registering, so a reused PID is never attached to the wrong process; a
  process that is already gone, or whose PID now belongs to another process, is
  an immediate exit event.
- **Watches armed or removed.** It watches `watches/` and each per-target
  directory (kqueue `EVFILT_VNODE` on macOS, inotify on Linux) and re-reads them,
  settling bursts for 50 ms. Reading takes no journal lock.
- **Fallback sweep.** Every five minutes (jittered) it re-checks all watched
  targets. This covers sessions whose liveness is not a process (endpoint- or
  socket-based adapters), registration failures and anything missed. Servers
  share one sweep per half interval through a stamp file,
  `watch-observer.stamp`, in the state directory. Where there is no exit source
  (an unsupported platform, an old kernel, or a source that fails) the sweep
  alone runs.

Latency is about immediate for process-owned sessions and up to the sweep
interval for endpoint-only liveness. With no watches the observer is blocked in
the kernel; its only timer is the sweep, which then finds nothing to do.

Before any notice, the exit decision is repeated under the journal lock from a
fresh read of the session: it must be retired or have positive exit evidence
(`sessionLiveness`), so a session that re-registered after the scan is never
reported as ended. Consuming a watch removes its record under that lock, so
several observers seeing one exit deliver one notice. Expired watches are
removed without a notice, only when their target is observed to have exited;
expired watches on live targets are left for `zalki prune`. The scan skips
anything it cannot trust (non-canonical names, symlinks, unreadable, future-schema
or unknown-adapter records), never follows symlinks, and never aborts on one.

The observer never affects tool calls. A failure is logged and skipped, a panic
stops only the observer, and shutdown cancels it and waits at most two seconds.
It logs one `watch.exit_observed` line per observed exit, with IDs only; nothing
is written to the MCP stream. MCP `list_agents` remains a read-only discovery
operation and never triggers native notice delivery. The CLI `zalki list` and an
applying `zalki prune` still deliver notices for exits they observe.

### Platform ports

The portable observer (`exit_observer.go`) depends only on the small `exitSource`
interface in `exit_source.go`. It is handle-based: nothing outside the platform
files touches file descriptors or `unix.*` types. Each platform implements it
once: `exit_source_darwin.go` (kqueue), `exit_source_linux.go` (pidfd, inotify and
epoll) and `exit_source_other.go` (reports the platform as unsupported, so the
observer sweeps). A Windows port needs `exit_source_windows.go`, mapping
`WatchProcess` to `OpenProcess(SYNCHRONIZE)` with `WaitForMultipleObjects` or
`RegisterWaitForSingleObject` and `WatchDir` to `ReadDirectoryChangesW`, plus
ports of the existing journal lock (`Flock`) and process probe
(`readProcess`). The observer itself does not change.

Notice delivery failures from the CLI paths warn on stderr while list/prune still
return their JSON results; journal observation errors can fail the command.
Unknown liveness and idle age never count as evidence of exit.

The lazy `state-formats.json` marker is ignored by rc.5. An unreadable or
unsupported marker fences writes with "upgrade zalki": the observer releases
its kernel watches, logs once and stays idle until shutdown, creating no watch directory or stamp and sending
no notices. For unreadable-marker recovery, see [Upgrading](../../README.md#upgrading).
