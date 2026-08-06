// Package version exposes build metadata shared by the CLI and protocol handshake.
package version

// These values may be overridden with -ldflags for release builds.
var (
	Version = "0.3.0-dev"
	Commit  = "unknown"
)
