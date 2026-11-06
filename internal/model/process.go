package model

// ProcessRef identifies a recorded process and its domain role. It is distinct
// from the raw OS identity used by process inspection mechanisms.
type ProcessRef struct {
	// PID is the recorded process identifier.
	PID int `json:"pid"`
	// Start distinguishes the original process from a later reuse of its PID.
	Start string `json:"start"`
	// Role records the process's role in its client.
	Role string `json:"role"`
}

// Liveness is the evidence-based state of a session or recorded process.
// Unknown is not evidence of exit and must never be treated as stale by age.
type Liveness string

const (
	// LivenessAlive identifies positive evidence of the recorded process.
	LivenessAlive Liveness = "alive"
	// LivenessExited identifies evidence that the recorded process has exited.
	LivenessExited Liveness = "exited"
	// LivenessUnknown identifies insufficient inspection evidence.
	LivenessUnknown Liveness = "unknown"
)
