package bus

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

const oldSessionGuidance = "Old records are not migrated. Start a new native session."

var (
	errSessionUnexpectedOwner = model.NewError(model.CodeUnavailable, "session record is not supported (unexpected process owner). "+oldSessionGuidance)
	errSessionMissingRoot     = model.NewError(model.CodeUnavailable, "session record is not supported (missing absolute config root). "+oldSessionGuidance)
	errSessionMissingPolicy   = model.NewError(model.CodeUnavailable, "session record is not supported (missing plugin policy). "+oldSessionGuidance)
)

func sessionIssueReason(err error) string {
	switch {
	case errors.Is(err, errSessionUnexpectedOwner):
		return "session_unexpected_owner"
	case errors.Is(err, errSessionMissingPolicy):
		return "session_missing_policy"
	case errors.Is(err, errSessionMissingRoot):
		return "session_missing_root"
	case model.ErrorCodeOf(err) == model.CodeUnsupportedSchema:
		return "session_unsupported_schema"
	case model.ErrorCodeOf(err) == model.CodeInvalidIdentity:
		return "session_identity_mismatch"
	default:
		return "session_unreadable"
	}
}

func sessionIssueLabel(reason string) string {
	switch reason {
	case "session_unexpected_owner":
		return "unexpected process owner"
	case "session_missing_policy":
		return "missing plugin policy"
	case "session_missing_root":
		return "missing absolute config root"
	case "session_unsupported_schema":
		return "unsupported schema"
	case "session_identity_mismatch":
		return "record identity mismatch"
	case "session_adapter_missing":
		return "adapter unavailable"
	default:
		return "unreadable record"
	}
}

// Only canonical filename-derived IDs and fixed labels reach terminal copy.
func diagnoseSessionIssues(report *DiagnosticReport, issues []SessionIssue) {
	if len(issues) == 0 {
		return
	}
	report.Notes = append(report.Notes, fmt.Sprintf("Skipped %d unreadable or unsupported session records. %s", len(issues), oldSessionGuidance))
	invalid := 0
	for _, issue := range issues {
		id, ok := watchNameID(strings.ReplaceAll(issue.ID, ":", "_"))
		if !ok || id != issue.ID {
			invalid++
			continue
		}
		report.Notes = append(report.Notes, fmt.Sprintf("Skipped session %s: %s.", id, sessionIssueLabel(issue.Reason)))
	}
	if invalid > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("Skipped %d session records with noncanonical filenames.", invalid))
	}
}
