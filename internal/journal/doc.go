// Package journal holds the generic, domain-free primitives beneath zalki's
// on-disk record store: bounded JSON reads, canonical record encoding, a
// field-preserving merge, schema headers, atomic replacement, directory sync
// and FD-level lock primitives.
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
// which order, what to recover, or when a format marker must be published.
// Its lock functions leave raw descriptor ownership and policy to the caller:
// they add no implicit retries, mutexes, caching, finalizers or close-on-exec.
package journal
