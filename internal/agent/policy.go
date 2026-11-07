package agent

import "github.com/zalkilabs/zalki/internal/model"

// NameAdapter supplies native session labels and their configuration location.
type NameAdapter interface {
	ConfigDir() string
	LookupName(model.Session, map[string]map[string]string) (string, string)
}

// HealthAdapter supplements common diagnostics with transport and listener health.
type HealthAdapter interface {
	Health(HealthHost, model.Session) (transport, listener string)
}

// RecoveryHintAdapter supplies a client-specific next step after core's common
// hints (retirement, exited owner, outdated build) found nothing to suggest.
type RecoveryHintAdapter interface {
	RecoveryHint(model.SessionDiagnostic) string
}

// CompatibilityAdapter supplements doctor with adapter-owned version evidence.
// Empty or malformed results leave the common fixture diagnosis unchanged.
type CompatibilityAdapter interface {
	Compatibility(version string) (status, hint string)
}

type HealthHost interface {
	Listener(model.Session) model.ListenerStatus
}
