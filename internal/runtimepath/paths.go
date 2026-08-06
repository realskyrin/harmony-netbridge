// Package runtimepath centralizes per-user runtime and log paths.
package runtimepath

import (
	"fmt"
	"os"
	"path/filepath"
)

const maxUnixSocketPath = 100

// Paths contains every local runtime file the daemon may own.
type Paths struct {
	RuntimeDir    string
	ControlSocket string
	StateFile     string
	LogDir        string
	LogFile       string
}

// Default resolves macOS user-specific locations without trusting the process working directory.
func Default() (Paths, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve user cache directory: %w", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve user home directory: %w", err)
	}
	return FromRoots(
		filepath.Join(cacheDir, "HarmonyNetBridge", "runtime"),
		filepath.Join(homeDir, "Library", "Logs", "HarmonyNetBridge"),
	)
}

// FromRoots constructs paths for production or isolated tests.
func FromRoots(runtimeDir, logDir string) (Paths, error) {
	paths := Paths{
		RuntimeDir:    filepath.Clean(runtimeDir),
		ControlSocket: filepath.Join(runtimeDir, "control.sock"),
		StateFile:     filepath.Join(runtimeDir, "state.json"),
		LogDir:        filepath.Clean(logDir),
		LogFile:       filepath.Join(logDir, "harmony-netbridge.log"),
	}
	if len(paths.ControlSocket) > maxUnixSocketPath {
		return Paths{}, fmt.Errorf("control socket path is too long for macOS: %s", paths.ControlSocket)
	}
	return paths, nil
}

// Ensure creates user-only runtime and log directories.
func (p Paths) Ensure() error {
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}
	if err := os.Chmod(p.RuntimeDir, 0o700); err != nil {
		return fmt.Errorf("protect runtime directory: %w", err)
	}
	if err := os.MkdirAll(p.LogDir, 0o700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	if err := os.Chmod(p.LogDir, 0o700); err != nil {
		return fmt.Errorf("protect log directory: %w", err)
	}
	return nil
}
