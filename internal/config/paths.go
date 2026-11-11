package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ResolvePaths selects config and state paths using flag, Zalki environment and
// XDG/default precedence. It reads no configuration and creates no directories.
func ResolvePaths(input Inputs) (Paths, error) {
	platform := input.GOOS
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform != "linux" && platform != "darwin" {
		return Paths{}, errors.New("config paths are supported on linux and darwin; use a supported build")
	}
	configFile, err := selectPath(input, "config", "ZALKI_CONFIG", "XDG_CONFIG_HOME", ".config", "config.json")
	if err != nil {
		return Paths{}, err
	}
	stateDir, err := selectPath(input, "state", "ZALKI_STATE", "XDG_STATE_HOME", filepath.Join(".local", "state"), "")
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		ConfigFile: configFile, StateDir: stateDir,
		ConfigDir:      filepath.Dir(configFile.Value),
		DefaultLogFile: filepath.Join(stateDir.Value, "logs", "zalki.jsonl"),
	}, nil
}

func selectPath(input Inputs, flag, variable, xdg, fallback, filename string) (Value[string], error) {
	choice := Value[string]{Source: "default", Origin: "home default"}
	fromEnv := environment(input, variable)
	switch {
	case hasFlag(input, flag):
		choice = Value[string]{Value: input.Flags[flag], Source: "flag", Origin: "--" + flag}
	case fromEnv != "":
		choice = Value[string]{Value: fromEnv, Source: "env", Origin: variable}
	default:
		base := environment(input, xdg)
		if filepath.IsAbs(base) {
			choice.Source, choice.Origin = "env", xdg
		} else {
			if !filepath.IsAbs(input.Home) {
				return choice, fmt.Errorf("%s needs an absolute home directory. Supply an absolute path or a valid absolute home/cwd", flag)
			}
			base = filepath.Join(input.Home, fallback)
		}
		choice.Value = filepath.Join(base, "zalki", filename)
	}
	abs, err := absolutePath(choice.Value, input.Cwd)
	if err != nil {
		return choice, fmt.Errorf("%s: %w", choice.Origin, err)
	}
	choice.Value = abs
	return choice, nil
}

func absolutePath(path, cwd string) (string, error) {
	if path == "" || !utf8.ValidString(path) || strings.IndexFunc(path, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		return "", errors.New("invalid path. Use a nonempty path without control characters (including newlines)")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if !filepath.IsAbs(cwd) {
		return "", errors.New("relative path requires an absolute cwd. Supply an absolute path or a valid absolute home/cwd")
	}
	return absolutePath(filepath.Join(cwd, path), "")
}

func hasFlag(input Inputs, name string) bool {
	_, exists := input.Flags[name]
	return exists
}

func environment(input Inputs, name string) string {
	lookup := input.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value, _ := lookup(name)
	return value
}
