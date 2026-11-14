// Package platform holds the OS mechanisms that differ between operating
// systems. It imports only the standard library.
package platform

// SocketID identifies one bound Unix socket file.
type SocketID struct {
	Path      string
	Device    uint64
	Inode     uint64
	ChangedNS int64
}
