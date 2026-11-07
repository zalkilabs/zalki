package model

import "encoding/json"

// SessionView carries credential-free session evidence for an adapter policy.
// The host constructs it with detached AdapterState; copying this value alone
// retains ordinary slice-sharing semantics.
type SessionView struct {
	Actor, Kind, Generation string
	State                   SessionState
	Retired                 bool
	AdapterState            json.RawMessage
}

// SessionDiagnostic is the diagnostic snapshot supplied to an adapter.
// The host assembles it; Owner remains a shared pointer and Receive a value.
type SessionDiagnostic struct {
	ID              string              `json:"id"`
	Kind            string              `json:"kind"`
	Name            string              `json:"name,omitempty"`
	State           SessionState        `json:"state"`
	Retired         bool                `json:"retired"`
	RetiredReason   string              `json:"retired_reason,omitempty"`
	Liveness        Liveness            `json:"liveness"`
	Owner           *ProcessRef         `json:"owner,omitempty"`
	UpdatedAt       string              `json:"updated_at"`
	TransportHealth string              `json:"transport_health"`
	ListenerHealth  string              `json:"listener_health,omitempty"`
	HookBuild       string              `json:"hook_build,omitempty"`
	HookBuildStatus string              `json:"hook_build_status"`
	RecoveryHint    string              `json:"recovery_hint,omitempty"`
	Receive         ReceiveCapabilities `json:"receive"`
}
