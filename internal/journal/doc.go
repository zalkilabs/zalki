// Package journal holds the generic, domain-free primitives beneath zalki's
// on-disk record store: bounded JSON reads, canonical record encoding, schema
// headers and durable batch intents. Atomic replacement and directory sync are
// in internal/fsutil.
//
// The package knows nothing about sessions, messages, record families, error
// codes or logging. It reports its own conditions through the sentinel
// [ErrInvalidFile] and the typed [SizeError] and
// [SchemaError]; every other failure is the standard library error, returned
// unchanged so that callers keep its text and its errors.Is behavior (for
// example [os.ErrNotExist]). A caller translates these values into its own
// error vocabulary.
//
// Callers own the transaction. This package does not decide when to lock, in
// which order, or what to recover. File locks are in internal/platform.
package journal
