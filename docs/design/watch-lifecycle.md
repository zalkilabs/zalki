# Completion watches and exit observation

A completion watch fires once when a target completes a turn or ends its
session through a lifecycle hook. Notices copy at most 200 UTF-8 bytes of its
last status line, ending at a word boundary when possible, with an ellipsis when truncated.
The status text is the target's unverified claim, not user approval.

A subscription created after a Stop handler has started remains armed for the
next completed Stop, so an already-running completion cannot consume a newer
watch. Expired watches are removed without a notice; watches last twelve hours.

There is no background process watcher. After a crash or kill without SessionEnd,
an ended notice fires only when an operator runs the explicit CLI `zalki list`
or an applying `zalki prune` and the target is found retired or positively exited.
Without such observation, the watch may expire silently. Dry-run prune does not
deliver notices. MCP `list_agents` remains a read-only discovery operation and
never triggers native notice delivery.

Adding a background observer, or choosing another automatic trigger for crash
notices, is follow-up behavior work. Unknown liveness and idle age never count
as evidence of exit.
