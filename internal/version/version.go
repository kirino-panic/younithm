// Package version holds the build's version string. Release images set it
// to the git tag and edge images to the short commit, via
// -ldflags "-X younithm/internal/version.Version=...". Local builds say "dev".
package version

var Version = "dev"
