// Package bus implements the experimental local agent messaging registry.
// macOS is the current release target; Linux remains an experimental port.
// It uses no daemon or external service.
// Store owns locked journal operations; Service coordinates native delivery,
// hooks, approval holds and lifecycle work. Watch and SetTask also live on
// Service as entry points, although they only use Store.
//
// # File layout
//
// Files are grouped by filename prefix; a prefix names a topic, not a type.
//
//	store*.go, identity.go    journal Store, record read/write, recovery, IDs
//	session*.go, discovery.go session registry, names and destination resolution, discovery
//	message*.go, service.go   send, inbox and message queries, reply, Service and delivery
//	message_open.go          open-message markers for hook scans and prune repair
//	envelope.go              sender labels for recipient rendering
//	commit.go, event*.go     session and message commits with their events, recovery
//	journal_view.go          scoped core reads and caller-owned write validation
//	plugin_inbox.go          injected admission/reservation and core message publication
//	endpoint.go              messaging-socket inspection and checks
//	hook*.go                 native hook entry point, lifecycle transitions, completed prompts
//	adapter*.go              registry access, adapter state, client compatibility, hook capture
//	listener.go, liveness.go  listener leases and evidence-based liveness
//	approval.go, flood*.go   approval holds and sender flood limits
//	watch*.go, exit_*.go     completion watches and the background exit observer
//	prune*.go                retirement and cleanup of stale records
//	process*.go, runtime.go  process identity and per-binary runtime tracking
//	diagnostics.go           health reports
//	logging.go               structured diagnostics
//	task.go                  task claims
//
// JournalView exposes core reads under the existing journal lock.
// WithWriteScope validates that a caller's package-owned writes use the live
// writable scope and the same state root, without exposing the Store.
// The two injected plugin calls admit the recipient and reserve inbox capacity
// under that lock before the core message is published.
// Open-message markers are made durable before an open record is written and
// removed after a closing write. Hook scans read them; prune repairs them. Stale
// markers are dropped only after syncing messages/.
//
// Shared record types, statuses and receipts live in internal/model. Plugin
// installations, feed replay, inbox policy and audit belong to internal/pluginfeed.
// Bus must not import that package or concrete clients; the CLI composes those
// implementations with the core.
package bus
