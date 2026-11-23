package bus

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

// errSessionUnsupported is the one error for a stored session that fails the
// shape checks of validateStoredSession. It names no cause: the record is
// skipped and left as it is.
var errSessionUnsupported = model.NewError(model.CodeUnavailable, "session record is not supported")

func sessionIssueReason(err error) string {
	switch {
	case errors.Is(err, errSessionUnsupported):
		return "session_unsupported"
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
	case "session_unsupported":
		return "unsupported record"
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
	report.Notes = append(report.Notes, fmt.Sprintf("Skipped %d unreadable or unsupported session records.", len(issues)))
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
