package model

// NoticeKind marks an automatic completion notice, rather than a message the
// target chose to send. It is a wire value, not a grant of user approval.
const NoticeKind = "notice"

// Message is the shared message record and response value. Its field tags
// distinguish persisted data from render-only metadata.
type Message struct {
	// PluginBinding is persisted by bus only; it never enters a public JSON projection.
	PluginBinding string `json:"-"`
	// BusyDelivery describes the target at this delivery attempt, not the
	// current state of a historical message. It is never persisted.
	BusyDelivery DeliveryTiming `json:"-"`
	// BodyPreview is opt-in sent history metadata, never journal content.
	BodyPreview string `json:"-"`
	Schema      int    `json:"schema"`
	ID          string `json:"id"`
	From        string `json:"from"`
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
	// Held marks a message this build queued for a permission prompt. Older
	// builds used the same status for a final refusal, and those records must
	// not be released later.
	Held           bool     `json:"held,omitempty"`
	Receipt        *Receipt `json:"receipt,omitempty"`
	AcknowledgedAt string   `json:"acknowledged_at,omitempty"`
}
