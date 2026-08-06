package runtimepath

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFromRootsAndEnsure(t *testing.T) {
	t.Parallel()
	base, err := os.MkdirTemp("/tmp", "hnb-paths-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	paths, err := FromRoots(filepath.Join(base, "runtime"), filepath.Join(base, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.RuntimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime permissions = %o", info.Mode().Perm())
	}
}

func TestRejectsLongUnixSocketPath(t *testing.T) {
	t.Parallel()
	_, err := FromRoots("/tmp/"+strings.Repeat("x", 120), "/tmp/log")
	if err == nil {
		t.Fatal("expected long socket path error")
	}
}
