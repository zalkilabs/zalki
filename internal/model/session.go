package model

import "encoding/json"

// Session is a persisted registration value. Its owner, endpoint and listener
// remain shared pointers when copied; receiving capabilities remain a value.
type Session struct {
	Schema       int             `json:"schema"`
	NativeID     string          `json:"native_id,omitempty"`
	AdapterState json.RawMessage `json:"adapter_state,omitempty"`
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	Name         string          `json:"name,omitempty"`
	NameSource   string          `json:"name_source,omitempty"`
	// Task is the agent's own unverified claim about its current work.
	// It is never an address and never changes routing.
	Task string `json:"task,omitempty"`
	// NativeConfigDir is captured in the owning CLI's hook environment, which
	// can differ from the sender MCP's filtered environment. Omitted in discovery.
	NativeConfigDir string       `json:"native_config_dir,omitempty"`
	CWD             string       `json:"cwd"`
	Generation      string       `json:"generation"`
	State           SessionState `json:"state"`
	UpdatedAt       string       `json:"updated_at"`
	Retired         bool         `json:"retired"`
	RetiredReason   string       `json:"retired_reason,omitempty"`
	// ApprovalHeld is how many messages are waiting for this session's permission
	// prompt. Hooks skip the message journal when it is zero.
	ApprovalHeld int         `json:"approval_held,omitempty"`
	Owner        *ProcessRef `json:"owner,omitempty"`
	Liveness     Liveness    `json:"liveness,omitempty"`
	HookBuild    string      `json:"hook_build,omitempty"`
	Endpoint     *Endpoint   `json:"endpoint,omitempty"`
	// Listener is the core-owned lease of a native listener.
	Listener *ListenerLease      `json:"listener,omitempty"`
	KeyHash  string              `json:"key_hash,omitempty"`
	Receive  ReceiveCapabilities `json:"receive"`
}
