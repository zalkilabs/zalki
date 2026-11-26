package clientconfig_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/clientconfig"
)

const profileVariable = "ZALKI_TEST_PROFILE_DIR"

func TestAbsoluteConfigDirKeepsOnlyCleanAbsoluteTextPaths(t *testing.T) {
	root := t.TempDir()
	for name, tc := range map[string]struct{ in, want string }{
		"clean path stays":         {root, root},
		"trailing slash is cut":    {root + string(filepath.Separator), root},
		"dots and doubles resolve": {root + "/a/../b/./c//", filepath.Join(root, "b", "c")},
		"trailing space is kept":   {root + "/x ", root + "/x "},
		"empty":                    {"", ""},
		"relative":                 {"profile", ""},
		"dot":                      {".", ""},
		"leading space":            {" " + root, ""},
		"home shortcut":            {"~/profile", ""},
		"invalid UTF-8":            {root + "/\xff", ""},
		"new line":                 {root + "/a\nb", ""},
		"tab":                      {root + "/a\tb", ""},
		"NUL":                      {root + "/a\x00b", ""},
		"DEL":                      {root + "/a\x7fb", ""},
		"C1 control":               {root + "/a\u0085b", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := clientconfig.AbsoluteConfigDir(tc.in); got != tc.want {
				t.Fatalf("AbsoluteConfigDir(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestConfigDirUsesTheOverrideExactlyAndNeverFallsBackFromAnInvalidOne(t *testing.T) {
	fallback := filepath.Join(t.TempDir(), "fallback")
	override := filepath.Join(t.TempDir(), "override")
	for name, tc := range map[string]struct {
		value    string
		fallback string
		want     string
	}{
		"empty uses the fallback":                  {"", fallback, fallback},
		"only spaces uses the fallback":            {"  ", fallback, fallback},
		"only tab and new line use the fallback":   {"\t\n", fallback, fallback},
		"override wins":                            {override, fallback, override},
		"override is cleaned":                      {override + "/./sub/..", fallback, override},
		"override keeps a trailing space":          {override + " ", fallback, override + " "},
		"relative override does not fall back":     {"relative", fallback, ""},
		"override with a leading space is refused": {" " + override, fallback, ""},
		"override with a control character":        {override + "/a\nb", fallback, ""},
		"override with invalid UTF-8":              {override + "/\xff", fallback, ""},
		"relative fallback is refused":             {"", "relative", ""},
		"empty fallback is refused":                {"", "", ""},
		"fallback with a control character":        {"", fallback + "/a\tb", ""},
		"a good override ignores a bad fallback":   {override, "relative", override},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(profileVariable, tc.value)
			if got := clientconfig.ConfigDir(profileVariable, tc.fallback); got != tc.want {
				t.Fatalf("ConfigDir(%q) with override %q = %q, want %q", tc.fallback, tc.value, got, tc.want)
			}
		})
	}
}

func TestConfigDirTreatsAMissingVariableLikeAnEmptyOne(t *testing.T) {
	fallback := filepath.Join(t.TempDir(), "fallback")
	t.Setenv(profileVariable, "placeholder")
	if err := os.Unsetenv(profileVariable); err != nil {
		t.Fatal(err)
	}
	if got := clientconfig.ConfigDir(profileVariable, fallback); got != fallback {
		t.Fatalf("ConfigDir = %q, want the fallback %q", got, fallback)
	}
}

func TestConfigDirReadsOnlyTheNamedVariable(t *testing.T) {
	fallback := filepath.Join(t.TempDir(), "fallback")
	other := filepath.Join(t.TempDir(), "other")
	t.Setenv(profileVariable, "")
	t.Setenv("ZALKI_TEST_OTHER_PROFILE_DIR", other)
	if got := clientconfig.ConfigDir(profileVariable, fallback); got != fallback {
		t.Fatalf("ConfigDir = %q, want the fallback %q", got, fallback)
	}
}
