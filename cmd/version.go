package cmd

import "runtime/debug"

// version is stamped into release builds with
// -ldflags "-X github.com/umccr/data-releaser/cmd.version=<version>".
var version = ""

// resolvedVersion is the release version, or the module version Go records for a
// `go install github.com/umccr/data-releaser@<version>` build, or "dev".
func resolvedVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func init() {
	rootCmd.Version = resolvedVersion()
}
