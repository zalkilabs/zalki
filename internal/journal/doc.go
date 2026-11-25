// Package journal holds the durable batch intents beneath zalki's on-disk
// record store: a redo batch of puts and deletes that is written first and
// applied in a fixed order. Bounded record reads, record encoding and the
// shared error values are in internal/statefile. Atomic replacement and
// directory sync are in internal/fsutil.
//
// The package knows nothing about sessions, messages, record families, error
// codes or logging. A malformed, unsupported or unsafe redo intent is reported
// as [statefile.ErrInvalidData]; a pending intent or target with different
// bytes is [ErrBatchConflict]. Every other failure is the standard library
// error, returned unchanged so that callers keep its text and its errors.Is
// behavior (for example [os.ErrNotExist]). A caller translates these values
// into its own error vocabulary.
//
// Callers own the transaction. This package does not decide when to lock, in
// which order, or what to recover. File locks are in internal/platform.
package journal
