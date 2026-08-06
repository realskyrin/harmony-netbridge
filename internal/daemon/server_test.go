package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/realskyrin/harmony-netbridge/internal/apperror"
	"github.com/realskyrin/harmony-netbridge/internal/control"
	"github.com/realskyrin/harmony-netbridge/internal/hdc"
	"github.com/realskyrin/harmony-netbridge/internal/protocol"
	"github.com/realskyrin/harmony-netbridge/internal/runtimepath"
	"github.com/realskyrin/harmony-netbridge/internal/state"
)

type fakeForwarder struct {
	added     chan hdc.Mapping
	mu        sync.Mutex
	removed   []hdc.Mapping
	removeErr error
}

func (f *fakeForwarder) AddReverse(_ context.Context, _ string, mapping hdc.Mapping) error {
	f.added <- mapping
	return nil
}

func (f *fakeForwarder) Remove(_ context.Context, _ string, mapping hdc.Mapping) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, mapping)
	return f.removeErr
}

func TestServerRetainsDiagnosticStateWhenMappingCleanupFails(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &fakeForwarder{
		added:     make(chan hdc.Mapping, 1),
		removeErr: errors.New("fixture cleanup failure"),
	}
	server, err := New(Config{
		Paths: paths, DeviceID: "secret-device-id", DeviceLabel: "device-redacted", Forwarder: forwarder,
	})
	if err != nil {
		t.Fatal(err)
	}

	runContext, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() { runResult <- server.Run(runContext) }()
	select {
	case <-forwarder.added:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not create reverse mapping")
	}
	cancel()
	select {
	case err := <-runResult:
		if err == nil {
			t.Fatal("Run() error = nil, want cleanup failure")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
	}

	snapshot, err := state.ReadFile(paths.StateFile)
	if err != nil {
		t.Fatalf("read retained state: %v", err)
	}
	if snapshot.Daemon != state.DaemonFailed || snapshot.LastErrorCode != string(apperror.CodeRPortFailed) {
		t.Fatalf("retained state = %#v", snapshot)
	}
	if _, err := os.Lstat(paths.ControlSocket); !os.IsNotExist(err) {
		t.Fatalf("control socket still exists after shutdown: %v", err)
	}
}

func TestServerHelloStatusAndStop(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &fakeForwarder{added: make(chan hdc.Mapping, 1)}
	server, err := New(Config{
		Paths:       paths,
		DeviceID:    "secret-device-id",
		DeviceLabel: "device-redacted",
		Forwarder:   forwarder,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	runResult := make(chan error, 1)
	go func() { runResult <- server.Run(context.Background()) }()

	var mapping hdc.Mapping
	select {
	case mapping = <-forwarder.added:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not create reverse mapping")
	}
	connection, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)), time.Second)
	if err != nil {
		t.Fatalf("connect device listener: %v", err)
	}

	helloPayload, err := protocol.MarshalJSONPayload(protocol.Hello{
		Role:              "control",
		Mode:              "phase1",
		AppVersion:        "test",
		SupportedVersions: []int{1},
		Capabilities:      []string{"control"},
		Message:           "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeHello, Sequence: 1, Payload: helloPayload}); err != nil {
		t.Fatal(err)
	}
	ackFrame, err := protocol.ReadFrame(connection)
	if err != nil {
		t.Fatalf("read HELLO_ACK: %v", err)
	}
	if ackFrame.Type != protocol.TypeHelloAck {
		t.Fatalf("frame type = %v, want HELLO_ACK", ackFrame.Type)
	}
	var ack protocol.HelloAck
	if err := protocol.UnmarshalJSONPayload(ackFrame.Payload, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Message != "world" || len(ack.SessionToken) != 32 {
		t.Fatalf("HELLO_ACK = %#v", ack)
	}

	requestContext, cancel := context.WithTimeout(context.Background(), time.Second)
	statusResponse, err := control.Call(requestContext, paths.ControlSocket, control.Request{Command: control.CommandStatus})
	cancel()
	if err != nil {
		t.Fatalf("status request: %v", err)
	}
	if statusResponse.State.Transport != state.TransportControlConnected {
		t.Fatalf("transport = %s, want %s", statusResponse.State.Transport, state.TransportControlConnected)
	}
	if statusResponse.State.Device != "device-redacted" {
		t.Fatalf("device = %q, want redacted label", statusResponse.State.Device)
	}

	stopContext, stopCancel := context.WithTimeout(context.Background(), time.Second)
	stopResponse, err := control.Call(stopContext, paths.ControlSocket, control.Request{Command: control.CommandStop})
	stopCancel()
	if err != nil || !stopResponse.OK {
		t.Fatalf("stop response = %#v, error = %v", stopResponse, err)
	}

	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
	}
	_ = connection.Close()

	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	if len(forwarder.removed) != 1 || forwarder.removed[0] != mapping {
		t.Fatalf("removed mappings = %#v, want only %#v", forwarder.removed, mapping)
	}
}

func TestServerRejectsSecondLiveInstanceBeforeAddingMapping(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-single-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	firstForwarder := &fakeForwarder{added: make(chan hdc.Mapping, 1)}
	first, err := New(Config{
		Paths: paths, DeviceID: "first", DeviceLabel: "device-first", Forwarder: firstForwarder,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstResult := make(chan error, 1)
	go func() { firstResult <- first.Run(context.Background()) }()
	select {
	case <-firstForwarder.added:
	case <-time.After(3 * time.Second):
		t.Fatal("first daemon did not start")
	}

	secondForwarder := &fakeForwarder{added: make(chan hdc.Mapping, 1)}
	second, err := New(Config{
		Paths: paths, DeviceID: "second", DeviceLabel: "device-second", Forwarder: secondForwarder,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = second.Run(context.Background())
	var appError *apperror.Error
	if !errors.As(err, &appError) || appError.Code != apperror.CodeDaemonRunning {
		t.Fatalf("second Run() error = %v, want %s", err, apperror.CodeDaemonRunning)
	}
	select {
	case mapping := <-secondForwarder.added:
		t.Fatalf("second daemon unexpectedly added mapping %#v", mapping)
	default:
	}

	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err = control.Call(stopContext, paths.ControlSocket, control.Request{Command: control.CommandStop})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-firstResult:
		if err != nil {
			t.Fatalf("first daemon Run() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first daemon did not stop")
	}
}
