package clientconfig

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ConfigDir returns the native profile folder of a client: the value of the
// environment variable when it is not empty or only white space, else
// fallback. The chosen value is used as it is, never trimmed. The result is
// empty when the chosen value is not a valid absolute path; an invalid
// override never falls back.
func ConfigDir(variable, fallback string) string {
	root := fallback
	if value := os.Getenv(variable); strings.TrimSpace(value) != "" {
		root = value
	}
	return AbsoluteConfigDir(root)
}

// AbsoluteConfigDir returns the cleaned form of root, or an empty string when
// root is not absolute, is not valid UTF-8 or holds a control character.
func AbsoluteConfigDir(root string) string {
	if !filepath.IsAbs(root) || !utf8.ValidString(root) || strings.ContainsFunc(root, unicode.IsControl) {
		return ""
	}
	return filepath.Clean(root)
}
