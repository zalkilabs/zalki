// Package procwatch reads operating-system process identities and observes
// process exits and directory changes. It contains raw OS mechanisms only;
// session ownership, liveness policy, persistence and delivery belong to callers.
// macOS is the current release target; Linux remains an experimental port.
package procwatch
