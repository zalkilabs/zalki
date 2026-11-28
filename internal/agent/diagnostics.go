package agent

// DiagnosticInput is the data the doctor gives an adapter that reports its own
// diagnostic section. It is plain data: the adapter reads the files of its own
// client, and everything that belongs to core is collected by the caller.
type DiagnosticInput struct {
	// Home is the user's home directory; StateDir and ConfigFile are Zalki's own
	// state directory and settings file; Binary is the zalki executable that the
	// installed hooks run.
	Home, StateDir, ConfigFile, Binary string
	// Found and Version describe this adapter's client as the doctor found it.
	Found   bool
	Version string
	// SessionScanComplete says whether the doctor read every session record;
	// HasSessions says whether one of them belongs to this adapter.
	SessionScanComplete, HasSessions bool
	// RecordReason is why the installation record could not be read as this
	// installation's ("" when it was, "missing_file" when there is none,
	// "unsafe_file", "invalid_data" or "ambiguous_ownership" otherwise).
	// RecordedTargets maps each installed file's declared path to the absolute
	// path the record holds for it.
	RecordReason    string
	RecordedTargets map[string]string
}

// DiagnosticsAdapter optionally reports a section of the doctor output about
// its client, under the returned key. The section must be JSON-encodable, must
// not start another process and must not write.
type DiagnosticsAdapter interface {
	Diagnose(DiagnosticInput) (key string, section any)
}

// Check is one finding of a client check. Status is "ok", "note" or
// "problem": a problem means messaging cannot work for this client; a note is
// advice. Fix is the one next step and is empty for "ok". Any other Status is
// shown as a problem.
type Check struct{ Name, Status, Fix string }

// CheckAdapter optionally reports the doctor's checklist lines for its
// client. It sees the same input as Diagnose, must not start another process
// and must not write. An adapter without it gets one note that points to the
// JSON section.
type CheckAdapter interface {
	Checks(DiagnosticInput) []Check
}
