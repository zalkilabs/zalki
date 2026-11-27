package install_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// These tests pin install.CheckLogFile as the CLI calls it before any command
// runs: client roots are checked first, uninstall passes no declaration, and a
// log file must not be a file that Zalki edits.

var clientRootEnv = []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"}

func declareFiles(files ...install.ConfigFile) install.Declare {
	return func(string, string) ([]install.ConfigFile, error) { return files, nil }
}

func settingsFile(draft, path, rootEnv string) install.ConfigFile {
	return install.ConfigFile{Path: path, Draft: draft, ConfigRootEnv: rootEnv}
}

func TestCheckLogFileRefusesEachClientRootWithAControlCharacter(t *testing.T) {
	for _, name := range clientRootEnv {
		t.Run(name, func(t *testing.T) {
			isolateConfigEnv(t)
			t.Setenv(name, "/tmp/root\nwith-newline")
			err := install.CheckLogFile(nil, "-", t.TempDir(), t.TempDir())
			if err == nil || !strings.HasPrefix(err.Error(), name+": invalid path.") {
				t.Fatalf("error = %v, want an invalid path error for %s", err, name)
			}
		})
	}
}

func TestCheckLogFileRefusesAClientRootThatIsNotValidUTF8(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("CODEX_HOME", "/tmp/root\xff")
	if err := install.CheckLogFile(nil, "", t.TempDir(), t.TempDir()); err == nil || !strings.HasPrefix(err.Error(), "CODEX_HOME: invalid path.") {
		t.Fatalf("error = %v, want an invalid path error for CODEX_HOME", err)
	}
}

// A root is refused for characters that are not graphic. Spaces that are not
// ASCII are graphic, so they pass; format and control characters do not.
func TestCheckLogFileAcceptsANonASCIISpaceInAClientRoot(t *testing.T) {
	for _, r := range []rune{0x00A0, 0x2003} {
		t.Run(fmt.Sprintf("U+%04X", r), func(t *testing.T) {
			isolateConfigEnv(t)
			t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/root"+string(r)+"with-space")
			if err := install.CheckLogFile(nil, "-", t.TempDir(), t.TempDir()); err != nil {
				t.Fatalf("error = %v, want none", err)
			}
		})
	}
}

func TestCheckLogFileRefusesAClientRootWithAnInvisibleCharacter(t *testing.T) {
	for _, r := range []rune{0x0085, 0x200B, 0x2028} {
		t.Run(fmt.Sprintf("U+%04X", r), func(t *testing.T) {
			isolateConfigEnv(t)
			t.Setenv("CURSOR_CONFIG_DIR", "/tmp/root"+string(r)+"hidden")
			err := install.CheckLogFile(nil, "-", t.TempDir(), t.TempDir())
			if err == nil || !strings.HasPrefix(err.Error(), "CURSOR_CONFIG_DIR: invalid path.") {
				t.Fatalf("error = %v, want an invalid path error for CURSOR_CONFIG_DIR", err)
			}
		})
	}
}

func TestCheckLogFileChecksClientRootsBeforeItReadsTheDeclarations(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("CURSOR_CONFIG_DIR", "/tmp/root\tTab")
	called := false
	declare := func(string, string) ([]install.ConfigFile, error) { called = true; return nil, nil }
	err := install.CheckLogFile(declare, filepath.Join(t.TempDir(), "zalki.log"), t.TempDir(), t.TempDir())
	if err == nil || !strings.HasPrefix(err.Error(), "CURSOR_CONFIG_DIR: invalid path.") || called {
		t.Fatalf("error = %v, declarations read = %v; want the root error and no declaration read", err, called)
	}
}

func TestCheckLogFileAcceptsValidClientRootsWithoutDeclarations(t *testing.T) {
	isolateConfigEnv(t)
	for _, name := range clientRootEnv {
		t.Setenv(name, filepath.Join(t.TempDir(), "client root"))
	}
	if err := install.CheckLogFile(nil, filepath.Join(t.TempDir(), "zalki.log"), t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("error = %v, want none", err)
	}
}

func TestCheckLogFileDoesNotReadDeclarationsForNoFileOrTheDiagnosticStream(t *testing.T) {
	for _, logFile := range []string{"-", ""} {
		t.Run("log file "+logFile, func(t *testing.T) {
			isolateConfigEnv(t)
			declare := func(string, string) ([]install.ConfigFile, error) { return nil, errors.New("declarations were read") }
			if err := install.CheckLogFile(declare, logFile, t.TempDir(), t.TempDir()); err != nil {
				t.Fatalf("error = %v, want none", err)
			}
		})
	}
}

func TestCheckLogFileAsksForTheDeclarationsOfTheBinaryAndTheState(t *testing.T) {
	isolateConfigEnv(t)
	var binary, state string
	declare := func(b, s string) ([]install.ConfigFile, error) { binary, state = b, s; return nil, nil }
	stateDir := t.TempDir()
	if err := install.CheckLogFile(declare, filepath.Join(t.TempDir(), "zalki.log"), stateDir, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if binary != "/zalki" || state != stateDir {
		t.Fatalf("declarations asked for binary %q and state %q, want %q and %q", binary, state, "/zalki", stateDir)
	}
}

func TestCheckLogFileReturnsTheErrorOfTheDeclarations(t *testing.T) {
	isolateConfigEnv(t)
	want := errors.New("no declarations")
	declare := func(string, string) ([]install.ConfigFile, error) { return nil, want }
	if err := install.CheckLogFile(declare, filepath.Join(t.TempDir(), "zalki.log"), t.TempDir(), t.TempDir()); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestCheckLogFileRefusesAFileThatZalkiEditsInTheHomeDirectory(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	target := filepath.Join(home, ".codex", "config.toml")
	declare := declareFiles(settingsFile("Codex", ".codex/config.toml", "CODEX_HOME"))
	err := install.CheckLogFile(declare, target, t.TempDir(), home)
	want := `--log-file points at a file Zalki edits for Codex ("` + target + `"); choose another log file`
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestCheckLogFileRefusesAFileThatAClientRootMovesZalkiTo(t *testing.T) {
	isolateConfigEnv(t)
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	target := filepath.Join(root, "config.toml")
	declare := declareFiles(settingsFile("Codex", ".codex/config.toml", "CODEX_HOME"))
	err := install.CheckLogFile(declare, target, t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), `("`+target+`")`) {
		t.Fatalf("error = %v, want a refusal that names %s", err, target)
	}
}

func TestCheckLogFileRefusesASymbolicLinkToAFileThatZalkiEdits(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	target := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "zalki.log")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
	declare := declareFiles(settingsFile("Cursor hooks", ".cursor/hooks.json", "CURSOR_CONFIG_DIR"))
	err := install.CheckLogFile(declare, link, t.TempDir(), home)
	if err == nil || !strings.Contains(err.Error(), "edits for Cursor hooks (\""+target+"\")") {
		t.Fatalf("error = %v, want a refusal for Cursor hooks that names %s", err, target)
	}
}

func TestCheckLogFileComparesEveryDeclaredFile(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	declare := declareFiles(
		settingsFile("Claude", ".claude.json", "CLAUDE_CONFIG_DIR"),
		settingsFile("Codex", ".codex/hooks.json", "CODEX_HOME"),
	)
	err := install.CheckLogFile(declare, filepath.Join(home, ".codex", "hooks.json"), t.TempDir(), home)
	if err == nil || !strings.Contains(err.Error(), "edits for Codex ") {
		t.Fatalf("error = %v, want a refusal for the second file, Codex", err)
	}
}

func TestCheckLogFileAcceptsALogFileThatZalkiDoesNotEdit(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	declare := declareFiles(
		settingsFile("Claude", ".claude.json", "CLAUDE_CONFIG_DIR"),
		settingsFile("Codex", ".codex/config.toml", "CODEX_HOME"),
	)
	for _, logFile := range []string{
		filepath.Join(t.TempDir(), "zalki.log"),
		filepath.Join(home, ".codex", "zalki.log"),
		filepath.Join(home, ".claude.json.log"),
	} {
		if err := install.CheckLogFile(declare, logFile, t.TempDir(), home); err != nil {
			t.Fatalf("log file %s: error = %v, want none", logFile, err)
		}
	}
}

func TestCheckLogFileReturnsAnErrorForAnInvalidTargetOfADeclaredFile(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("CODEX_HOME", "relative/root")
	declare := declareFiles(settingsFile("Codex", ".codex/config.toml", "CODEX_HOME"))
	err := install.CheckLogFile(declare, filepath.Join(t.TempDir(), "zalki.log"), t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "CODEX_HOME must be an absolute configuration directory") {
		t.Fatalf("error = %v, want the absolute directory error", err)
	}
}

func TestCheckLogFileReturnsAnErrorWhenTheLogFileCannotBeInspected(t *testing.T) {
	isolateConfigEnv(t)
	plain := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	declare := declareFiles(settingsFile("Codex", ".codex/config.toml", "CODEX_HOME"))
	if err := install.CheckLogFile(declare, filepath.Join(plain, "zalki.log"), t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("error = none, want the error of the file system")
	}
}
