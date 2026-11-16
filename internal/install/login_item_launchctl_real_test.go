package install

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// realLaunchctlFixture reads a sanitized real launchctl output; "gui/UID"
// becomes this test's domain.
func realLaunchctlFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "launchctl", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(string(raw), "gui/UID = {", domain()+" = {", 1)
}

// R7-H1: the real outputs of a current Mac parse. `print gui/UID` is larger
// than the old 64 KiB limit and has status words such as "(pe)"; only its
// services block counts. `print-disabled` is read by its one "disabled
// services" block; another block after it is skipped.
func TestPluginLoginItemRealLaunchctlOutputs(t *testing.T) {
	t.Run("domain", checkRealDomainOutput)
	t.Run("disabled", checkRealDisabledOutput)
	t.Run("through the real adapter", checkRealOutputThroughAdapter)
}

func checkRealDomainOutput(t *testing.T) {
	out := realLaunchctlFixture(t, "print-domain-macos26.txt")
	labels, complete := domainServices([]byte(out))
	if !complete || len(labels) != 436 || slices.Contains(labels, loginItemLabel) {
		t.Fatalf("a real service list was not read completely: complete=%t, %d labels", complete, len(labels))
	}
	mine := strings.Replace(out, "\tcom.example.s2\n", "\t"+loginItemLabel+"\n", 1)
	if labels, complete := domainServices([]byte(mine)); !complete || !slices.Contains(labels, loginItemLabel) {
		t.Fatal("the job was not found in a real service list")
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if _, complete := domainServices([]byte(strings.Join(lines[:len(lines)-1], "\n"))); complete {
		t.Fatal("a truncated service list was read as complete")
	}
	if _, complete := domainServices([]byte(strings.Replace(out, "\t\t       0      0 \tcom.example.s5\n", "\t\tsomething new\n", 1))); complete {
		t.Fatal("an unknown service row was accepted")
	}
}

func checkRealDisabledOutput(t *testing.T) {
	for _, name := range []string{"print-disabled-macos26.txt", "print-disabled-two-blocks-synthetic.txt"} {
		out := realLaunchctlFixture(t, name)
		if disabled, known := disabledState([]byte(out)); !known || disabled {
			t.Fatalf("a real disabled list was not read (%s): known=%t disabled=%t", name, known, disabled)
		}
		mine := strings.Replace(out, `"com.example.q1" => disabled`, `"`+loginItemLabel+`" => disabled`, 1)
		if disabled, known := disabledState([]byte(mine)); !known || !disabled {
			t.Fatalf("the job's disablement was not seen (%s)", name)
		}
	}
	two := realLaunchctlFixture(t, "print-disabled-two-blocks-synthetic.txt")
	if _, known := disabledState([]byte(two + two)); known {
		t.Fatal("two disabled lists were read as one")
	}
}

// checkRealOutputThroughAdapter: the real-size output passes the adapter's
// output limit (a fake launchctl prints it).
func checkRealOutputThroughAdapter(t *testing.T) {
	out := realLaunchctlFixture(t, "print-domain-macos26.txt")
	file := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(file, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	old := launchctlPath
	t.Cleanup(func() { launchctlPath = old })
	launchctlPath = fakeLaunchctlScript(t, "/bin/cat "+file)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, exit, err := realLaunchctl(ctx, "print", domain())
	if err != nil || exit != 0 || string(got) != out {
		t.Fatalf("a real-size launchctl output did not pass the adapter: %d bytes, exit %d, %v", len(got), exit, err)
	}
}
