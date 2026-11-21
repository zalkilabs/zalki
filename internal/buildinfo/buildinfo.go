// Package buildinfo holds the release version of the binary. Release builds
// set Version with the linker (see .goreleaser.yaml).
package buildinfo

// Version is set by release builds with -X.
var Version = "0.1.0-experimental"

// WithBuild returns Version followed by the first 12 characters of the loaded
// binary's digest, or Version alone when the digest is shorter.
func WithBuild(loadedBuild string) string {
	if len(loadedBuild) >= 12 {
		return Version + "+" + loadedBuild[:12]
	}
	return Version
}
