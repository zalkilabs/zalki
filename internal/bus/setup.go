package bus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// Configuration returns only paths declared by the compiled-in adapters.
func Configuration(binary, state string) ([]ConfigFile, error) {
	return defaultRegistry.configuration(binary, state)
}

func WriteSetup(binary, state, out, home string) error {
	files, err := Configuration(binary, state)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(home) {
		return errors.New("home path must be absolute")
	}
	targets := map[string]string{}
	seen := map[string]bool{}
	for _, file := range files {
		target, err := file.target(home)
		if err != nil {
			return err
		}
		if seen[target] || target == filepath.Join(state, "installation.json") {
			return errors.New("configuration targets overlap each other or installation state")
		}
		seen[target] = true
		targets[file.Draft] = target
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		return err
	}
	for _, file := range files {
		var data []byte
		if file.JSON != nil {
			data, err = json.MarshalIndent(file.JSON, "", "  ")
			if err != nil {
				return err
			}
			data = append(data, '\n')
		} else {
			data = []byte(file.TOML)
		}
		if err := exclusiveFile(filepath.Join(out, file.Draft), data); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(targets, "", "  ")
	if err != nil {
		return err
	}
	return exclusiveFile(filepath.Join(out, "targets.json"), append(data, '\n'))
}

func exclusiveFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func mergeConfig(existing []byte, patch map[string]any) ([]byte, error) {
	current := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &current); err != nil {
			return nil, err
		}
		if current == nil {
			return nil, errors.New("settings must be a JSON object")
		}
	}
	for key, value := range patch {
		switch key {
		case "version":
			if old, exists := current[key]; exists && old != float64(1) {
				return nil, errors.New("unsupported existing Cursor hook schema version")
			}
			current[key] = value
		case "mcpServers":
			servers, ok := current[key].(map[string]any)
			if current[key] != nil && !ok {
				return nil, errors.New("existing mcpServers is not an object")
			}
			if servers == nil {
				servers = map[string]any{}
			}
			wanted := value.(map[string]any)["zalki"]
			if old, exists := servers["zalki"]; exists {
				a, _ := json.Marshal(old)
				b, _ := json.Marshal(wanted)
				if string(a) != string(b) {
					return nil, errors.New("an existing zalki MCP server differs; review it before replacing")
				}
			}
			servers["zalki"] = wanted
			current[key] = servers
		case "hooks":
			hooks, ok := current[key].(map[string]any)
			if current[key] != nil && !ok {
				return nil, errors.New("existing hooks is not an object")
			}
			if hooks == nil {
				hooks = map[string]any{}
			}
			for event, addition := range value.(map[string]any) {
				entries, ok := hooks[event].([]any)
				if hooks[event] != nil && !ok {
					return nil, errors.New("existing hook event is not an array")
				}
				wanted, _ := json.Marshal(addition.([]any)[0])
				found := false
				for _, entry := range entries {
					encoded, _ := json.Marshal(entry)
					if string(encoded) == string(wanted) {
						found = true
					}
				}
				if !found {
					entries = append(entries, addition.([]any)[0])
				}
				hooks[event] = entries
			}
			current[key] = hooks
		default:
			return nil, errors.New("unexpected settings key")
		}
	}
	data, err := json.MarshalIndent(current, "", "  ")
	return append(data, '\n'), err
}

type configEdit struct {
	path          string
	before, after []byte
	existed       bool
	backup        string
	remove        bool
}

// Install applies the reviewed user-config additions and records their ownership.
func Install(binary, state, home string) ([]string, error) {
	report, err := ManageInstallation(InstallationRequest{Action: "install", Binary: binary, StateDir: state, HomeDir: home, Apply: true})
	return report.Paths, err
}

func configBytes(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4*1024*1024 {
		return nil, false, errors.New("refusing non-regular or oversized settings file")
	}
	b, err := os.ReadFile(path)
	return b, true, err
}

func atomicBytes(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".zalki-config-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
