// Package config resolves process-local Zalki settings without initializing a
// journal, logger, client integration or cache.
package config

import "time"

// Inputs contains invocation-local dependencies. Flags contains only explicitly
// changed flags, without their leading --. Home and Cwd are supplied by the caller.
// An empty GOOS uses the running platform and a nil LookupEnv uses os.LookupEnv.
type Inputs struct {
	Home      string
	Cwd       string
	GOOS      string
	LookupEnv func(string) (string, bool)
	Flags     map[string]string
}

// Value records the selected value and its default, file, env or flag source.
type Value[T any] struct {
	Value  T      `json:"value"`
	Source string `json:"source"`
	Origin string `json:"origin"`
}

// Paths contains absolute bootstrap paths. Resolving them has no filesystem effects.
type Paths struct {
	ConfigFile     Value[string] `json:"config_file"`
	StateDir       Value[string] `json:"state_dir"`
	ConfigDir      string        `json:"config_dir"`
	DefaultLogFile string        `json:"default_log_file"`
}

// Settings is an invocation snapshot. Duration values are typed for callers,
// which can format them with time.Duration.String in inspection output.
type Settings struct {
	MaxBodyBytes Value[int]
	LogLevel     Value[string]
	LogFile      Value[string]
	// LogFileFromState preserves lazy initialization of the default state sink,
	// including an explicitly empty log-file override.
	LogFileFromState bool
	RetiredOlderThan Value[time.Duration]
}

// Snapshot owns resolved values without retaining environment or flag maps.
type Snapshot struct {
	Paths      Paths
	Settings   Settings
	FileExists bool
}
