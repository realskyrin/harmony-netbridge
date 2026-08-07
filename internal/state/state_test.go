package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreAndAtomicFile(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 6, 1, 2, 3, 0, time.UTC)
	store := NewStore(NewStarting(start, "device-redacted", 27183))
	updated := store.Update(start.Add(time.Second), func(snapshot *Snapshot) {
		snapshot.Daemon = DaemonRunning
		snapshot.Transport = TransportPortReady
		snapshot.HostPort = 54321
		snapshot.Proxy = ProxySnapshot{
			Enabled:     true,
			Status:      ProxyActive,
			ListenPort:  8080,
			UpstreamURL: "http://127.0.0.1:3128",
			SSLInsecure: true,
		}
	})
	if updated.UpdatedAt != start.Add(time.Second) {
		t.Fatalf("UpdatedAt = %s", updated.UpdatedAt)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFile(path, updated); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != 3 || loaded.Daemon != DaemonRunning || loaded.Transport != TransportPortReady ||
		loaded.HostPort != 54321 || loaded.Proxy.Status != ProxyActive || loaded.Proxy.ListenPort != 8080 ||
		loaded.Proxy.UpstreamURL != "http://127.0.0.1:3128" || !loaded.Proxy.SSLInsecure {
		t.Fatalf("loaded state = %#v", loaded)
	}
}
