package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriter(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bridge.log")
	writer, err := OpenRotatingWriter(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("first-record\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("second-record\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(previous), "first-record") || !strings.Contains(string(active), "second-record") {
		t.Fatalf("unexpected rotation: previous=%q active=%q", previous, active)
	}
}

func TestRotatingWriterRemainsUsableAfterRotationFailure(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bridge.log")
	writer, err := OpenRotatingWriter(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := writer.Write([]byte("first-record\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".1", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".1", "blocker"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("second-record\n")); err == nil {
		t.Fatal("rotation unexpectedly succeeded with a directory at the previous-log path")
	}
	if err := os.RemoveAll(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("third-record\n")); err != nil {
		t.Fatalf("writer did not recover after rotation failure: %v", err)
	}
}
