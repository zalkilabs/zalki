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

The portable observer (`internal/bus/exit_observer.go`) consumes the raw
`procwatch.Source` interface and `procwatch.NewSource` from
`internal/procwatch/source.go`. OS handles and `unix.*` operations belong to that
leaf, with `source_darwin.go` (kqueue), `source_linux.go` (pidfd, inotify and
epoll) and `source_other.go` (unsupported, so the observer sweeps). There is no
remaining bus `exit_source.go` forwarding wrapper. A Windows source would map
`WatchProcess` to `OpenProcess(SYNCHRONIZE)` with `WaitForMultipleObjects` or
`RegisterWaitForSingleObject` and `WatchDir` to `ReadDirectoryChangesW`, plus
ports of the existing journal lock and `procwatch.Read` process probe.
The observer retains domain liveness translation, journal fences, notice
decisions and source/Close ownership; `procwatch.Identity` is a raw PID/start
identity, distinct from `model.ProcessRef`. Unknown evidence is still not exit.

Notice delivery failures from the CLI paths warn on stderr while list/prune still
return their JSON results; journal observation errors can fail the command.
Unknown liveness and idle age never count as evidence of exit.

### Retired global format fence

The earlier rc.5 `state-formats.json` fencing description is historical.
[Commit 396669e](https://github.com/zalkilabs/zalki/commit/396669e645860e9a941811e1dbe15b81d77a5602),
an ancestor of the inspected source, removed that fence and its observer/MCP
marker tests. The current observer creates its watch directory, opens the raw
source and synchronizes without a global marker check; it does not promise to
stay idle or send no notices because such a marker is unreadable.

Current record handling is operation-specific: see
`internal/bus/unsupported_record_test.go` for skip/count/warn and exact-read
cases. This is not a guarantee that arbitrary mixed-build writes are safe.
The source-lifetime and unknown-liveness rules above remain unchanged. Archive,
StageB/release authority and any future migration policy remain separate open
follow-ups, not a fence implementation requested or deferred by this docs change.
Do not delete marker or state files to bypass an unsupported record.
