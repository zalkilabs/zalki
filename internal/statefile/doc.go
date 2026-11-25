// Package statefile holds the domain-free primitives for one stored record:
// bounded JSON reads, canonical record encoding, strict decoding and schema
// headers. Atomic replacement and directory sync are in internal/fsutil.
//
// The package knows nothing about sessions, messages, record families, error
// codes or logging. It reports its own conditions through the sentinels
// [ErrInvalidFile] and [ErrInvalidData] and the typed [SizeError] and
// [SchemaError]; every other failure is the standard library error, returned
// unchanged so that callers keep its text and its errors.Is behavior (for
// example [os.ErrNotExist]). A caller translates these values into its own
// error vocabulary.
//
// Callers own the transaction. This package does not decide when to lock, in
// which order, or what to recover. File locks are in internal/platform.
package statefile
