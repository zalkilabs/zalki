// Package install plans and applies zalki's native client configuration edits,
// tracks installation ownership and writes reviewable setup drafts. Plugin
// source builds and plugin journal commits belong to plugins and pluginfeed.
package install

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigFile declares one client configuration file an adapter owns.
type ConfigFile struct {
	Path  string
	Draft string
	JSON  map[string]any
	TOML  string
	// ConfigRootEnv relocates this file's basename; Path remains its stable ownership key.
	ConfigRootEnv string
}

func (file ConfigFile) target(home string) (string, error) {
	root := os.Getenv(file.ConfigRootEnv)
	if err := checkClientRoot(file.ConfigRootEnv, root); err != nil {
		return "", err
	}
	if file.ConfigRootEnv == "" || strings.TrimSpace(root) == "" {
		return filepath.Join(home, file.Path), nil
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%s must be an absolute configuration directory", file.ConfigRootEnv)
	}
	return filepath.Join(root, filepath.Base(file.Path)), nil
}

func backupStamp() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
