package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// Load resolves one immutable snapshot. A missing selected file uses defaults.
// File structure is checked strictly before flag/env overrides are applied.
func Load(input Inputs) (Snapshot, error) {
	input = captureInputs(input)
	paths, err := ResolvePaths(input)
	if err != nil {
		return Snapshot{}, err
	}
	raw, exists, err := readConfig(paths.ConfigFile.Value)
	if err != nil {
		return Snapshot{Paths: paths}, err
	}
	return loadContent(input, paths, raw, exists)
}

// loadContent shares strict decoding and source resolution with the editor.
// The selected path stays unchanged so relative values resolve against it.
func loadContent(input Inputs, paths Paths, raw []byte, exists bool) (Snapshot, error) {
	result := Snapshot{Paths: paths, FileExists: exists}
	content := fileContent{path: paths.ConfigFile.Value, raw: raw, locations: map[string]int{}}
	var file fileSettings
	var err error
	if exists {
		file, err = decodeConfig(content)
		if err != nil {
			return result, err
		}
	}
	result.Settings, err = resolveSettings(input, paths, content, fileValues(file))
	return result, err
}

func captureInputs(input Inputs) Inputs {
	env := map[string]string{}
	for _, key := range []string{"ZALKI_CONFIG", "ZALKI_STATE", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "ZALKI_LOG_LEVEL", "ZALKI_LOG_FILE"} {
		env[key] = environment(input, key)
	}
	input.LookupEnv = func(key string) (string, bool) { value, ok := env[key]; return value, ok }
	input.Flags = maps.Clone(input.Flags)
	return input
}

func fileValues(file fileSettings) map[string]string {
	values := map[string]string{}
	if file.Codex.AllowQueueFallback != nil {
		values["codex.allow_queue_fallback"] = strconv.FormatBool(*file.Codex.AllowQueueFallback)
	}
	if file.Messages.MaxBodyBytes != nil {
		values["messages.max_body_bytes"] = strconv.Itoa(*file.Messages.MaxBodyBytes)
	}
	for key, value := range map[string]*string{
		"logging.level": file.Logging.Level, "logging.file": file.Logging.File,
		"prune.retired_older_than": file.Prune.RetiredOlderThan,
	} {
		if value != nil {
			values[key] = *value
		}
	}
	return values
}

func selectValue(input Inputs, content fileContent, file map[string]string, key, flag, variable, fallback string) Value[string] {
	if value, changed := input.Flags[flag]; changed {
		return Value[string]{Value: value, Source: "flag", Origin: "--" + flag}
	}
	if value := environment(input, variable); value != "" {
		return Value[string]{Value: value, Source: "env", Origin: variable}
	}
	return fileValue(content, file, key, fallback)
}

func fileValue(content fileContent, file map[string]string, key, fallback string) Value[string] {
	if value, present := file[key]; present {
		return Value[string]{Value: value, Source: "file", Origin: content.path + "#" + key}
	}
	return Value[string]{Value: fallback, Source: "default", Origin: "default " + key}
}

func resolveSettings(input Inputs, paths Paths, content fileContent, file map[string]string) (Settings, error) {
	var result Settings
	queueFallback := fileValue(content, file, "codex.allow_queue_fallback", "false")
	result.CodexAllowQueueFallback = Value[bool]{Value: queueFallback.Value == "true", Source: queueFallback.Source, Origin: queueFallback.Origin}
	body := fileValue(content, file, "messages.max_body_bytes", strconv.Itoa(model.MaxMessageBodyBytes))
	bytes, err := strconv.Atoi(body.Value)
	if err != nil || bytes < 1 || bytes > model.MaxMessageBodyBytes {
		return result, settingError(content, "messages.max_body_bytes", body, "body limit is out of range", "Set messages.max_body_bytes to an integer from 1 through 16384")
	}
	result.MaxBodyBytes = Value[int]{Value: bytes, Source: body.Source, Origin: body.Origin}
	level := selectValue(input, content, file, "logging.level", "log-level", "ZALKI_LOG_LEVEL", "info")
	level.Value = strings.ToLower(level.Value)
	switch level.Value {
	case "debug", "info", "warn", "error", "off":
		result.LogLevel = level
	default:
		return result, settingError(content, "logging.level", level, "invalid log level", "Use debug, info, warn, error, or off")
	}
	result.LogFile, result.LogFileFromState, err = resolveLogFile(input, paths, content, file)
	if err != nil {
		return result, err
	}
	retention := fileValue(content, file, "prune.retired_older_than", "168h")
	duration, err := time.ParseDuration(retention.Value)
	if err != nil || duration < 0 {
		return result, settingError(content, "prune.retired_older_than", retention, "invalid retired-session retention", "Use a nonnegative Go duration no greater than 2562047h47m16.854775807s")
	}
	result.RetiredOlderThan = Value[time.Duration]{Value: duration, Source: retention.Source, Origin: retention.Origin}
	return result, nil
}

func resolveLogFile(input Inputs, paths Paths, content fileContent, file map[string]string) (Value[string], bool, error) {
	value := selectValue(input, content, file, "logging.file", "log-file", "ZALKI_LOG_FILE", "")
	if value.Value == "" {
		value.Value = paths.DefaultLogFile
		value.Origin += " (derived from state_dir)"
		return value, true, nil
	}
	if value.Value == "-" {
		return value, false, nil
	}
	base := input.Cwd
	if value.Source == "file" {
		base = paths.ConfigDir
	}
	abs, err := absolutePath(value.Value, base)
	if err != nil {
		return value, false, settingError(content, "logging.file", value, "invalid log path", "Use a nonempty path without control characters (including newlines) and an absolute path or valid absolute cwd")
	}
	value.Value = filepath.Clean(abs)
	return value, false, nil
}

func settingError(content fileContent, key string, value Value[string], problem, fix string) error {
	if value.Source == "file" {
		return content.atKey(key, problem, fix)
	}
	return fmt.Errorf("%s, key %s: %s. %s", value.Origin, key, problem, fix)
}
