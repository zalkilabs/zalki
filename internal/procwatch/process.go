package procwatch

// Info is a snapshot of one operating-system process.
// Start identifies a process incarnation independently of PID reuse.
type Info struct {
	// PID is the process ID.
	PID int
	// PPID is the parent process ID.
	PPID int
	// Start is the platform-specific start fingerprint.
	Start string
	// Name is the operating system's process name.
	Name string
	// UID is the owning user's numeric ID.
	UID uint32
	// Zombie reports a process that has exited but has not been reaped.
	Zombie bool
}

// Identity identifies one process incarnation, without session or adapter policy.
// Values are comparable and may be used as map keys.
type Identity struct {
	// PID is the process ID.
	PID int
	// Start distinguishes the original process from a reused PID.
	Start string
}
