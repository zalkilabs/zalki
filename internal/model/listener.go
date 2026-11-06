package model

// Endpoint records the identity of a messaging socket. Inspecting or checking
// that socket remains the caller's responsibility; this value performs no IO.
type Endpoint struct {
	// Path is the socket's absolute path.
	Path string `json:"path"`
	// Device identifies the socket's filesystem device.
	Device uint64 `json:"device"`
	// Inode identifies the socket on its device.
	Inode uint64 `json:"inode"`
	// ChangedNS is the socket's recorded change time in nanoseconds.
	ChangedNS int64 `json:"changed_ns"`
}

// ListenerLease identifies one armed listener of a registration. The live
// resource and ownership checks remain with the caller holding its lock.
type ListenerLease struct {
	// ID identifies the armed lease.
	ID string `json:"id"`
	// Generation binds the lease to one registration generation.
	Generation string `json:"generation"`
	// Endpoint is the socket identity stored by value with the lease.
	Endpoint Endpoint `json:"endpoint"`
}

// ListenerStatus records the core's evidence about a native listener.
type ListenerStatus string

const (
	// ListenerNone identifies an absent listener lease and lifetime lock.
	ListenerNone ListenerStatus = "none"
	// ListenerRunning identifies a valid bound endpoint and matching held lease.
	ListenerRunning ListenerStatus = "running"
	// ListenerStale identifies a lease whose endpoint or lifetime lock is gone.
	ListenerStale ListenerStatus = "stale"
	// ListenerUnleased identifies a held lifetime lock without a recorded lease.
	ListenerUnleased ListenerStatus = "unleased"
)
