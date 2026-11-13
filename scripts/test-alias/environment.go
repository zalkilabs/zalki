package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func buildEnvironment(ctx context.Context, repo string, f aliasFixture, out, diagnostic io.Writer) (string, []string, error) {
	env, err := aliasEnvironment(f)
	if err != nil {
		return "", nil, err
	}
	launcher, err := exec.LookPath("go")
	if err != nil {
		return "", nil, err
	}
	launcher, err = filepath.EvalSymlinks(launcher)
	if err != nil {
		return "", nil, err
	}
	var raw bytes.Buffer
	if err := runChild(ctx, repo, env, &raw, diagnostic, launcher, "env", "-json", "GOCACHE", "GOMODCACHE", "GOROOT"); err != nil {
		return "", nil, err
	}
	paths := map[string]string{}
	if err := json.Unmarshal(raw.Bytes(), &paths); err != nil {
		return "", nil, err
	}
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOROOT"} {
		path, err := canonicalDirectory(paths[key])
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", key, err)
		}
		paths[key] = path
	}
	paths["GOTOOLCHAIN"], paths["PATH"] = "local", filepath.Join(paths["GOROOT"], "bin")+":/usr/bin:/bin"
	env = replaceEnvironment(env, paths, nil)
	if _, err := fmt.Fprintf(out, "Canonical caches: GOCACHE=%s; GOMODCACHE=%s\n", paths["GOCACHE"], paths["GOMODCACHE"]); err != nil {
		return "", nil, err
	}
	return filepath.Join(paths["GOROOT"], "bin", "go"), env, nil
}

// Never enumerate or forward the ambient environment. Only these Go controls
// and cache locations are read; arbitrary GOFLAGS cannot filter the full suite.
func aliasEnvironment(f aliasFixture) ([]string, error) {
	values := map[string]string{"PATH": "/usr/bin:/bin", "HOME": filepath.Join(f.root, "home"), "CODEX_HOME": filepath.Join(f.root, "codex"), "CLAUDE_CONFIG_DIR": filepath.Join(f.root, "claude"), "CURSOR_CONFIG_DIR": filepath.Join(f.root, "cursor")}
	for _, key := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		if err := f.opened.Mkdir(filepath.Base(values[key]), 0700); err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT", "CGO_ENABLED"} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	if err := defaultAliasCaches(values); err != nil {
		return nil, err
	}
	values["TMPDIR"], values["GOTMPDIR"] = f.real, f.real
	values["GOFLAGS"], values["GOMAXPROCS"] = "-p=1 -mod=readonly", "2"
	values["GOENV"], values["GOWORK"], values["GOTELEMETRY"] = "off", "off", "off"
	values["GOPROXY"], values["GOSUMDB"], values["GOTOOLCHAIN"] = "off", "off", "auto"
	values["GONOPROXY"], values["GONOSUMDB"], values["GOPRIVATE"], values["GOVCS"] = "none", "none", "", "*:off"
	values["ZALKI_LIVE_SMOKE"] = "0"
	return replaceEnvironment(nil, values, nil), nil
}

func defaultAliasCaches(values map[string]string) error {
	if values["GOCACHE"] == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		values["GOCACHE"] = filepath.Join(cache, "go-build")
	}
	if values["GOPATH"] == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		values["GOPATH"] = filepath.Join(home, "go")
	}
	if values["GOMODCACHE"] == "" {
		values["GOMODCACHE"] = filepath.Join(filepath.SplitList(values["GOPATH"])[0], "pkg", "mod")
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("expected an absolute cache/toolchain directory")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func replaceEnvironment(original []string, overrides map[string]string, absent []string) []string {
	values := map[string]string{}
	for _, entry := range original {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	for _, key := range absent {
		delete(values, key)
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
