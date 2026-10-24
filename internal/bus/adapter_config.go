package bus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
	if file.ConfigRootEnv == "" || strings.TrimSpace(root) == "" {
		return filepath.Join(home, file.Path), nil
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%s must be an absolute configuration directory", file.ConfigRootEnv)
	}
	return filepath.Join(root, filepath.Base(file.Path)), nil
}

type configurationAdapter interface {
	Configuration(string, []string) []ConfigFile
}

func (r *Registry) configuration(binary, state string) ([]ConfigFile, error) {
	if !filepath.IsAbs(binary) || !filepath.IsAbs(state) {
		return nil, errors.New("binary and state paths must be absolute")
	}
	args := []string{"--state", state}
	files := []ConfigFile{}
	paths, drafts := map[string]bool{}, map[string]bool{"targets.json": true}
	for _, kind := range r.Kinds() {
		adapter := r.adapters[kind]
		configuration, ok := adapter.(configurationAdapter)
		if !ok {
			continue
		}
		for _, file := range configuration.Configuration(binary, append([]string{}, args...)) {
			if !safeConfigPath(file.Path) || !safeConfigPath(file.Draft) || filepath.Base(file.Draft) != file.Draft {
				return nil, fmt.Errorf("%s declares an unsafe configuration path", kind)
			}
			if paths[file.Path] || drafts[file.Draft] {
				return nil, errors.New("agent configuration paths and draft names must be unique")
			}
			if (file.JSON == nil) == (file.TOML == "") {
				return nil, errors.New("configuration must declare exactly one JSON patch or TOML fragment")
			}
			if file.JSON != nil && len(file.JSON) == 0 {
				return nil, errors.New("configuration JSON patch must not be empty")
			}
			paths[file.Path], drafts[file.Draft] = true, true
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		return nil, errors.New("no adapter declares installation configuration")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func safeConfigPath(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path && path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator))
}

func hookCommand(binary string, args []string, kind string) string {
	parts := append([]string{binary}, args...)
	parts = append(parts, "hook", "--agent", kind)
	for i := range parts {
		parts[i] = shellQuote(parts[i])
	}
	return strings.Join(parts, " ")
}

func hookConfig(events map[string]string, command string, flat bool) map[string]any {
	hooks := map[string]any{}
	for native, common := range events {
		handler := map[string]any{"command": command, "timeout": 5}
		var entry any
		if flat {
			if common == "Stop" {
				handler["loop_limit"] = 5
			}
			entry = handler
		} else {
			handler["type"] = "command"
			entry = map[string]any{"hooks": []any{handler}}
		}
		hooks[native] = []any{entry}
	}
	return map[string]any{"hooks": hooks}
}

func mcpConfig(binary string, args []string, typed bool) map[string]any {
	server := map[string]any{"command": binary, "args": append(append([]string{}, args...), "mcp")}
	if typed {
		server["type"] = "stdio"
	}
	return map[string]any{"mcpServers": map[string]any{"zalki": server}}
}
