// Package bus implements the experimental local agent messaging registry.
// macOS is the current release target; Linux remains an experimental port.
// It uses no daemon, file watcher, or external service.
// Store owns locked journal operations; Service coordinates operations that may
// deliver through an adapter. Watch and SetTask also live on Service as entry
// points, although they only use Store.
package bus
