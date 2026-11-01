package bus

import (
	"encoding/json"
)

type Endpoint struct {
	Path      string `json:"path"`
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	ChangedNS int64  `json:"changed_ns"`
}

type Session struct {
	Schema       int             `json:"schema"`
	NativeID     string          `json:"native_id,omitempty"`
	AdapterState json.RawMessage `json:"adapter_state,omitempty"`
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	Name         string          `json:"name,omitempty"`
	NameSource   string          `json:"name_source,omitempty"`
	// NativeConfigDir is captured in the owning CLI's hook environment, which
	// can differ from the sender MCP's filtered environment. Omitted in discovery.
	NativeConfigDir string       `json:"native_config_dir,omitempty"`
	CWD             string       `json:"cwd"`
	Generation      string       `json:"generation"`
	State           SessionState `json:"state"`
	UpdatedAt       string       `json:"updated_at"`
	Retired         bool         `json:"retired"`
	RetiredReason   string       `json:"retired_reason,omitempty"`
	Owner           *ProcessRef  `json:"owner,omitempty"`
	Liveness        string       `json:"liveness,omitempty"`
	HookBuild       string       `json:"hook_build,omitempty"`
	Endpoint        *Endpoint    `json:"endpoint,omitempty"`
	// Listener is the core-owned lease of a native listener. Records that carry
	// one are written as leaseSessionSchema, which older builds refuse to read.
	Listener *ListenerLease      `json:"listener,omitempty"`
	KeyHash  string              `json:"key_hash,omitempty"`
	Receive  ReceiveCapabilities `json:"receive"`
}

// ListenerLease identifies the one armed listener of a registration. Only the
// process holding the matching in-memory Lease may confirm, validate or clear it.
type ListenerLease struct {
	ID         string   `json:"id"`
	Generation string   `json:"generation"`
	Endpoint   Endpoint `json:"endpoint"`
}

type ReceiveCapabilities struct {
	IdleWake         bool           `json:"idle_wake"`
	Delivery         string         `json:"delivery"`
	Inbox            InboxMode      `json:"inbox"`
	BusyDelivery     DeliveryTiming `json:"busy_delivery"`
	LifecycleHooks   bool           `json:"lifecycle_hooks"`
	HookDelivery     bool           `json:"hook_delivery"`
	RequiresListener bool           `json:"requires_listener"`
	RequiresEndpoint bool           `json:"requires_endpoint"`
	Experimental     bool           `json:"experimental"`
}

type Message struct {
	// BusyDelivery describes the target at this delivery attempt, not the
	// current state of a historical message. It is never persisted.
	BusyDelivery DeliveryTiming `json:"-"`
	Schema       int            `json:"schema"`
	ID           string         `json:"id"`
	From         string         `json:"from"`
	// Kind is "notice" for an automatic completion notice. Empty means an agent wrote the message.
	Kind string `json:"kind,omitempty"`
	// Snapshot the sender's label so queued envelopes remain stable after renames.
	FromName    string `json:"from_name,omitempty"`
	FromLabel   string `json:"from_label,omitempty"`
	To          string `json:"to"`
	RequestedTo string `json:"requested_to,omitempty"`
	Body        string `json:"body"`
	// BodyOmitted and BodyDeferred are response-only flags. Native bodies stay
	// in their queued delivery until acknowledged.
	BodyOmitted      bool           `json:"body_omitted,omitempty"`
	BodyDeferred     bool           `json:"body_deferred,omitempty"`
	ReplyTo          string         `json:"reply_to,omitempty"`
	TargetGeneration string         `json:"target_generation"`
	CreatedAt        string         `json:"created_at"`
	Status           DeliveryStatus `json:"status"`
	Receipt          *Receipt       `json:"receipt,omitempty"`
	AcknowledgedAt   string         `json:"acknowledged_at,omitempty"`
}
