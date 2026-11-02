# Flood protection and reply loop proposal

Agent sends and replies share core admission limits. Over-limit calls return
`rate_limited` with the limiting scope, retry delay, and UTC retry time. They
do not reserve a message, queue it for later, or acknowledge an incoming reply
parent. Completion watch notices remain exempt and one-shot.

## Defaults and observed traffic

| Scope | Short bucket burst | Refill per minute | Hard rolling hour cap |
| --- | ---: | ---: | ---: |
| One sender to one target | 12 | 12 | 120 |
| All senders to one target | 48 | 40 | 400 |

These are constants in `internal/bus/flood.go`. A burst is extra immediate
capacity, not a strict rolling-minute count: a full pair bucket can admit its
initial 12 and then replenish 12 over the next minute. The hourly caps count
admissions in the preceding hour and expire each admission individually.

A private copy of the journal on 2026-11-01 contained 272 messages across 77
directed pairs and 42 targets. There were no notices or malformed records. The
four historical approval refusals were included, making the measurement
conservative. No message bodies, names, capabilities, or machine paths are
included here.

| Distribution | Samples | p50 | p95 | p99 | Maximum |
| --- | ---: | ---: | ---: | ---: | ---: |
| Pair count in nonempty fixed UTC minutes | 263 | 1 | 1 | 2 | 3 |
| Target count in nonempty fixed UTC minutes | 258 | 1 | 2 | 2 | 3 |
| Pair count over the preceding 60 seconds at each arrival | 272 | 1 | 2 | 3 | 3 |
| Target count over the preceding 60 seconds at each arrival | 272 | 1 | 2 | 3 | 3 |
| Pair count over the preceding hour at each arrival | 272 | 2 | 8 | 10 | 10 |
| Target count over the preceding hour at each arrival | 272 | 2 | 14 | 20 | 22 |

Percentiles use nearest rank. Empty fixed-minute bins are excluded; rolling
windows include the current arrival and exclude the exact lower boundary. The
p99 rolling-minute pair count is 25% of its refill rate and burst. Target p99
is 7.5% of its refill rate and 6.25% of its burst. Hourly p99 is 8.3% of the pair
cap and 5% of the target cap; even the target maximum uses only 5.5% of its cap.
Replaying all 272 timestamped metadata records through these limits admits all
272 and refuses none.

At 6–12 total messages per minute, an alternating two-agent loop sends about
3–6 per directed pair. The pair hourly cap stops that sustained traffic after
roughly 20–40 minutes from empty history. Lower-rate loops can continue within
the quotas, so rate limiting does not replace a semantic loop guard.

## Storage and retry behavior

One private, versioned file per target contains both short buckets and at most
400 hourly admissions. Sender keys are derived identifiers. Idle short-bucket
entries expire after a minute, when they would be fully refilled. Each send
reads and atomically replaces this bounded file under the existing journal
lock; it never scans message history. Clock rollback cannot grant tokens or
move the watermark backwards. Corrupt or unsupported quota state fails closed.

Exact committed-message lookup runs before quota. The hourly admission entries
also retain the message ID and sender so a retry after quota IO but before
message or reply-intent commit reuses its admission. Quota is reserved first:
disk failure can temporarily consume capacity without delivery, but cannot
allow uncounted delivery after restart. A pending admission expires after an
hour; retries of committed messages stay exempt regardless of age.

Approval holds use quota at reservation and are not charged again when a hook
releases them. Quota denial clears tentative hold metadata. Refused reply
records for unavailable destinations also use quota, preventing that fallback
from growing the journal without a bound on its rate. The quota directory is
auxiliary state: it does not change message or session schemas or add tools.
Prune removes quota files when their session record is absent or is deleted by
retention in the same run, reporting file names in `deleted_flood`. Dry-run
reports the same candidates without deleting them. Existing session records,
including recently retired ones, retain their quota. Unrelated files and
symlinks are left alone.
As with other shared-state features, all active clients must run the same build.

## Proposed human reset loop guard

The normalized `HookInput` has no common authenticated human-origin field.
Codex explicitly handles completed native queued prompts through
`CompletedNativePrompt`; Cursor maps `beforeSubmitPrompt` into the same common
prompt event. Treating every `UserPromptSubmit` as a human reset would assume
that delivered agent prompts cannot reach it, which the current common contract
does not establish. Prompt text matching alone is not a trusted origin signal.

Require a verified origin event from each adapter before implementing resets.
Core would store a human-prompt epoch per session and a consecutive-message
count per unordered pair, keyed by both participants' epochs. Increment only
on a fresh admitted agent message or reply; retries, recovery, queue release,
and watch notices do not increment it. A trusted human prompt on either side
starts a new epoch; new message IDs or reply roots do not. Once the count is
exhausted, refuse without queueing or ACK until that reset occurs.

Choose the threshold after measuring conversation epochs rather than guessing
from wall-clock message rates. Required adapter probes must cover actual human
input, native queued/synthetic agent input, resume, compaction, and session
restart. Regression tests must show that agent input cannot reset the guard,
human input can, and new reply roots cannot bypass it. The implemented minute
and hourly limits remain the enforcement floor while this origin contract is
unresolved.
