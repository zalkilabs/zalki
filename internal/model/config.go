package model

// ConfigDeclaration is one client configuration file that an adapter owns, as
// the adapter declares it. The registry validates declarations; the command
// line converts them for the installer, which decides where and how to write.
//
// Exactly one of JSON and TOML is set. The declaration owns its JSON map: no
// code between the adapter and the installer copies it.
type ConfigDeclaration struct {
	// Path is the file's path relative to the home directory. It is the stable
	// ownership key even when ConfigRootEnv moves the file.
	Path string
	// Draft is the file name of the reviewable draft written by setup.
	Draft string
	// JSON is the patch merged into a JSON settings file.
	JSON map[string]any
	// TOML is the fragment added to a TOML settings file.
	TOML string
	// ConfigRootEnv names an environment variable holding a directory that
	// relocates the file's base name. It is empty when the file cannot move.
	ConfigRootEnv string
}
