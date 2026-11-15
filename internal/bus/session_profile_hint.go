package bus

import (
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
)

const nativeProfileRequirement = "native profile root must be absolute; check the selected profile environment. Use UTF-8 without control characters; a leading ~ is not expanded."

func nativeProfileHint(adapter agent.Adapter) string {
	notes := adapter.Descriptor().Notes
	if len(notes) == 0 {
		return nativeProfileRequirement
	}
	return nativeProfileRequirement + " " + strings.Join(notes, " ")
}

// ConfigDir cannot identify which choice failed. Do not infer a bad variable or
// inspect a stored target here: this is the doctor's own environment selection.
func diagnoseNativeProfile(report *DiagnosticReport, adapter agent.Adapter) {
	names, ok := adapter.(agent.NameAdapter)
	if ok && !validNativeConfigRoot(names.ConfigDir()) {
		report.Notes = append(report.Notes, "Selected client profile is invalid. "+nativeProfileRequirement)
	}
}
