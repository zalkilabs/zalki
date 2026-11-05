// Package bus implements the experimental local agent messaging registry.
// macOS is the current release target; Linux remains an experimental port.
// It uses no daemon or external service.
// Store owns locked journal operations; Service coordinates operations that may
// deliver through an adapter. Watch and SetTask also live on Service as entry
// points, although they only use Store.
//
// # File layout
//
// Files are grouped by filename prefix; a prefix names a topic, not a type.
//
//	model.go, status.go       shared types, delivery and session states, errors
//	store*.go, identity.go    journal Store, record read/merge/write, state-format fencing, IDs
//	session*.go, discovery.go session registry, names and destination resolution, discovery
//	message*.go, service.go   send, inbox and message queries, reply, Service and delivery receipts
//	hook*.go                  native hook entry point, lifecycle transitions, completed prompts
//	adapter*.go               adapter contracts, client compatibility, hook contract capture
//	listener.go, liveness.go  listener leases and evidence-based liveness
//	approval.go, flood*.go    approval holds and sender flood limits
//	watch*.go, exit_*.go      completion watches and the background exit observer
//	prune*.go                 retirement and cleanup of stale records
//	process*.go, runtime.go   process identity and per-binary runtime tracking
//	diagnostics.go, logging.go, task.go
//
// Core must not name or import any client; boundary_test.go enforces this.
package bus
