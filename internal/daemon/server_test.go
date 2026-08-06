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
	packetrelay "github.com/realskyrin/harmony-netbridge/internal/relay"
	"github.com/realskyrin/harmony-netbridge/internal/runtimepath"
	"github.com/realskyrin/harmony-netbridge/internal/state"
)

type fakeForwarder struct {
	added     chan hdc.Mapping
	mu        sync.Mutex
	removed   []hdc.Mapping
	removeErr error
}

type fakePacketRelay struct {
	injected  chan []byte
	output    chan []byte
	closeOnce sync.Once
	stats     packetrelay.Stats
}

func newFakePacketRelay() *fakePacketRelay {
	return &fakePacketRelay{injected: make(chan []byte, 1), output: make(chan []byte, 1)}
}

func (r *fakePacketRelay) Inject(packet []byte) error {
	r.injected <- append([]byte(nil), packet...)
	return nil
}

func (r *fakePacketRelay) Output() <-chan []byte { return r.output }

func (r *fakePacketRelay) Snapshot() packetrelay.Stats { return r.stats }

func (r *fakePacketRelay) Close() error {
	r.closeOnce.Do(func() { close(r.output) })
	return nil
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

func TestNewRejectsUnsafeVPNMTU(t *testing.T) {
	t.Parallel()
	forwarder := &fakeForwarder{added: make(chan hdc.Mapping, 1)}
	for _, mtu := range []int{575, 1501} {
		if _, err := New(Config{DeviceID: "device", DeviceLabel: "redacted", MTU: mtu, Forwarder: forwarder}); err == nil {
			t.Fatalf("New accepted MTU %d", mtu)
		}
	}
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

func TestServerContinuesReconnectCountAfterUncleanExit(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-recovered-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	previous := state.NewStarting(time.Now(), "device-redacted", DefaultDevicePort)
	previous.Daemon = state.DaemonRunning
	previous.Reconnects = 2
	previous.HostPort = 45_678
	if err := state.WriteFile(paths.StateFile, previous); err != nil {
		t.Fatal(err)
	}
	forwarder := &fakeForwarder{added: make(chan hdc.Mapping, 1)}
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
		t.Fatal("recovered daemon did not start")
	}
	if got := server.store.Get().Reconnects; got != 3 {
		t.Fatalf("Reconnects = %d, want 3", got)
	}
	forwarder.mu.Lock()
	if len(forwarder.removed) != 1 || forwarder.removed[0] != (hdc.Mapping{DevicePort: DefaultDevicePort, HostPort: 45_678}) {
		forwarder.mu.Unlock()
		t.Fatalf("stale mappings removed before recovery = %#v", forwarder.removed)
	}
	forwarder.mu.Unlock()
	cancel()
	select {
	case runError := <-runResult:
		if runError != nil {
			t.Fatal(runError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovered daemon did not stop")
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
	stopFrame, err := protocol.ReadFrame(connection)
	if err != nil || stopFrame.Type != protocol.TypeStopRequest {
		t.Fatalf("STOP_REQUEST = %#v, error = %v", stopFrame, err)
	}
	if err := protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeStopAck, Sequence: 2}); err != nil {
		t.Fatalf("write STOP_ACK: %v", err)
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

func TestServerVPNControlDataAndPacketRelay(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-daemon-vpn-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &fakeForwarder{added: make(chan hdc.Mapping, 1)}
	packetRelay := newFakePacketRelay()
	server, err := New(Config{
		Paths:       paths,
		DeviceID:    "secret-device-id",
		DeviceLabel: "device-redacted",
		Forwarder:   forwarder,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		RelayFactory: func() (PacketRelay, error) {
			return packetRelay, nil
		},
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
	dial := func() net.Conn {
		connection, dialError := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)), time.Second)
		if dialError != nil {
			t.Fatal(dialError)
		}
		return connection
	}

	controlConnection := dial()
	defer controlConnection.Close()
	controlHello, _ := protocol.MarshalJSONPayload(protocol.Hello{
		Role: "control", Mode: "vpn", AppVersion: "test", SupportedVersions: []int{1},
		Capabilities: []string{"control", "data"}, Message: "hello",
	})
	if err := protocol.WriteFrame(controlConnection, protocol.Frame{Type: protocol.TypeHello, Sequence: 1, Payload: controlHello}); err != nil {
		t.Fatal(err)
	}
	ackFrame, err := protocol.ReadFrame(controlConnection)
	if err != nil {
		t.Fatal(err)
	}
	var ack protocol.HelloAck
	if ackFrame.Type != protocol.TypeHelloAck || protocol.UnmarshalJSONPayload(ackFrame.Payload, &ack) != nil {
		t.Fatalf("invalid VPN control ACK: %#v", ackFrame)
	}

	dataConnection := dial()
	defer dataConnection.Close()
	dataHello, _ := protocol.MarshalJSONPayload(protocol.DataHello{SessionToken: ack.SessionToken, Role: "data"})
	if err := protocol.WriteFrame(dataConnection, protocol.Frame{Type: protocol.TypeDataHello, Sequence: 1, Payload: dataHello}); err != nil {
		t.Fatal(err)
	}
	dataAck, err := protocol.ReadFrame(dataConnection)
	if err != nil || dataAck.Type != protocol.TypeDataAck || dataAck.Sequence != 1 {
		t.Fatalf("DATA_ACK = %#v, error = %v", dataAck, err)
	}

	devicePacket := []byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 17, 0, 0, 10, 0, 0, 2, 1, 1, 1, 1}
	if err := protocol.WriteFrame(dataConnection, protocol.Frame{Type: protocol.TypeIPPacket, Sequence: 2, Payload: devicePacket}); err != nil {
		t.Fatal(err)
	}
	select {
	case injected := <-packetRelay.injected:
		if string(injected) != string(devicePacket) {
			t.Fatalf("injected packet = %x", injected)
		}
	case <-time.After(time.Second):
		t.Fatal("packet was not injected into relay")
	}
	macPacket := []byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 6, 0, 0, 1, 1, 1, 1, 10, 0, 0, 2}
	packetRelay.output <- macPacket
	returnedFrame, err := protocol.ReadFrame(dataConnection)
	if err != nil || returnedFrame.Type != protocol.TypeIPPacket || returnedFrame.Sequence != 2 ||
		string(returnedFrame.Payload) != string(macPacket) {
		t.Fatalf("returned packet frame = %#v, error = %v", returnedFrame, err)
	}

	statusPayload, _ := protocol.MarshalJSONPayload(protocol.VPNStatusPayload{State: "ACTIVE", Message: "ready"})
	if err := protocol.WriteFrame(controlConnection, protocol.Frame{Type: protocol.TypeVPNStatus, Sequence: 2, Payload: statusPayload}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		statusContext, cancel := context.WithTimeout(context.Background(), time.Second)
		response, callError := control.Call(statusContext, paths.ControlSocket, control.Request{Command: control.CommandStatus})
		cancel()
		if callError == nil && response.State.Transport == state.TransportDataConnected && response.State.VPN == state.VPNActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not report active VPN: %#v, error = %v", response, callError)
		}
		time.Sleep(10 * time.Millisecond)
	}

	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err = control.Call(stopContext, paths.ControlSocket, control.Request{Command: control.CommandStop})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	stopFrame, err := protocol.ReadFrame(controlConnection)
	if err != nil || stopFrame.Type != protocol.TypeStopRequest {
		t.Fatalf("STOP_REQUEST = %#v, error = %v", stopFrame, err)
	}
	select {
	case err := <-runResult:
		t.Fatalf("daemon stopped before STOP_ACK: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := protocol.WriteFrame(controlConnection, protocol.Frame{Type: protocol.TypeStopAck, Sequence: 3}); err != nil {
		t.Fatalf("write STOP_ACK: %v", err)
	}
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestServerNegotiatesMTUAndHeartbeatsBothChannels(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-daemon-heartbeat-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &fakeForwarder{added: make(chan hdc.Mapping, 1)}
	packetRelay := newFakePacketRelay()
	packetRelay.stats = packetrelay.Stats{
		PacketsFromDevice: 3, BytesFromDevice: 300,
		PacketsToDevice: 2, BytesToDevice: 200,
		TCPFlows: 1, UDPFlows: 1, DNSQueries: 1,
	}
	server, err := New(Config{
		Paths: paths, DeviceID: "secret-device-id", DeviceLabel: "device-redacted",
		MTU: 1280, Forwarder: forwarder,
		HeartbeatInterval: 20 * time.Millisecond,
		HeartbeatTimeout:  2 * time.Second,
		RelayFactory:      func() (PacketRelay, error) { return packetRelay, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runResult := make(chan error, 1)
	go func() { runResult <- server.Run(context.Background()) }()
	mapping := <-forwarder.added
	dial := func() net.Conn {
		connection, dialError := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)), time.Second)
		if dialError != nil {
			t.Fatal(dialError)
		}
		_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
		return connection
	}

	controlConnection := dial()
	defer controlConnection.Close()
	controlHello, _ := protocol.MarshalJSONPayload(protocol.Hello{
		Role: "control", Mode: "vpn", AppVersion: "test", SupportedVersions: []int{1},
		Capabilities: []string{"control", "data"}, Message: "hello",
	})
	if err := protocol.WriteFrame(controlConnection, protocol.Frame{Type: protocol.TypeHello, Sequence: 1, Payload: controlHello}); err != nil {
		t.Fatal(err)
	}
	controlAck, err := protocol.ReadFrame(controlConnection)
	if err != nil {
		t.Fatal(err)
	}
	var acknowledgement protocol.HelloAck
	if protocol.UnmarshalJSONPayload(controlAck.Payload, &acknowledgement) != nil || acknowledgement.MTU != 1280 {
		t.Fatalf("HELLO_ACK = %#v", acknowledgement)
	}

	dataConnection := dial()
	defer dataConnection.Close()
	dataHello, _ := protocol.MarshalJSONPayload(protocol.DataHello{SessionToken: acknowledgement.SessionToken, Role: "data"})
	if err := protocol.WriteFrame(dataConnection, protocol.Frame{Type: protocol.TypeDataHello, Sequence: 1, Payload: dataHello}); err != nil {
		t.Fatal(err)
	}
	if frame, readError := protocol.ReadFrame(dataConnection); readError != nil || frame.Type != protocol.TypeDataAck {
		t.Fatalf("DATA_ACK = %#v, error = %v", frame, readError)
	}
	statusPayload, _ := protocol.MarshalJSONPayload(protocol.VPNStatusPayload{State: "ACTIVE", Message: "ready"})
	if err := protocol.WriteFrame(controlConnection, protocol.Frame{Type: protocol.TypeVPNStatus, Sequence: 2, Payload: statusPayload}); err != nil {
		t.Fatal(err)
	}

	controlPing, err := protocol.ReadFrame(controlConnection)
	if err != nil || controlPing.Type != protocol.TypePing || len(controlPing.Payload) != 8 {
		t.Fatalf("control PING = %#v, error = %v", controlPing, err)
	}
	if err := protocol.WriteFrame(controlConnection, protocol.Frame{Type: protocol.TypePong, Sequence: 3, Payload: controlPing.Payload}); err != nil {
		t.Fatal(err)
	}
	dataPing, err := protocol.ReadFrame(dataConnection)
	if err != nil || dataPing.Type != protocol.TypePing || len(dataPing.Payload) != 8 {
		t.Fatalf("data PING = %#v, error = %v", dataPing, err)
	}
	if err := protocol.WriteFrame(dataConnection, protocol.Frame{Type: protocol.TypePong, Sequence: 2, Payload: dataPing.Payload}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		statusContext, cancel := context.WithTimeout(context.Background(), time.Second)
		response, callError := control.Call(statusContext, paths.ControlSocket, control.Request{Command: control.CommandStatus})
		cancel()
		if callError == nil && !response.State.ControlHeartbeatAt.IsZero() && !response.State.DataHeartbeatAt.IsZero() {
			if response.State.MTU != 1280 || response.State.Relay.DNSQueries != 1 || response.State.Relay.TCPFlows != 1 {
				t.Fatalf("Phase 3 state = %#v", response.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeats were not reported: %#v, error = %v", response, callError)
		}
		time.Sleep(5 * time.Millisecond)
	}

	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err = control.Call(stopContext, paths.ControlSocket, control.Request{Command: control.CommandStop})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	deviceSequence := uint32(4)
	for {
		frame, readError := protocol.ReadFrame(controlConnection)
		if readError != nil {
			t.Fatal(readError)
		}
		if frame.Type == protocol.TypePing {
			if err := protocol.WriteFrame(controlConnection, protocol.Frame{Type: protocol.TypePong, Sequence: deviceSequence, Payload: frame.Payload}); err != nil {
				t.Fatal(err)
			}
			deviceSequence = nextSequence(deviceSequence)
			continue
		}
		if frame.Type != protocol.TypeStopRequest {
			t.Fatalf("unexpected control frame %#v", frame)
		}
		if err := protocol.WriteFrame(controlConnection, protocol.Frame{Type: protocol.TypeStopAck, Sequence: deviceSequence}); err != nil {
			t.Fatal(err)
		}
		break
	}
	select {
	case runError := <-runResult:
		if runError != nil {
			t.Fatal(runError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestServerAcceptsSingleDeviceReconnectAfterUnexpectedLoss(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-daemon-reconnect-")
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
		Paths: paths, DeviceID: "secret-device-id", DeviceLabel: "device-redacted", Forwarder: forwarder,
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
		t.Fatal("daemon did not start")
	}
	dialControl := func() (net.Conn, protocol.HelloAck) {
		connection, dialError := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)), time.Second)
		if dialError != nil {
			t.Fatal(dialError)
		}
		hello, _ := protocol.MarshalJSONPayload(protocol.Hello{
			Role: "control", Mode: "vpn", AppVersion: "test", SupportedVersions: []int{1},
			Capabilities: []string{"control", "data"}, Message: "hello",
		})
		if writeError := protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeHello, Sequence: 1, Payload: hello}); writeError != nil {
			t.Fatal(writeError)
		}
		frame, readError := protocol.ReadFrame(connection)
		if readError != nil {
			t.Fatal(readError)
		}
		var acknowledgement protocol.HelloAck
		if protocol.UnmarshalJSONPayload(frame.Payload, &acknowledgement) != nil {
			t.Fatalf("invalid HELLO_ACK: %#v", frame)
		}
		return connection, acknowledgement
	}

	first, _ := dialControl()
	_ = first.Close()
	deadline := time.Now().Add(time.Second)
	for server.store.Get().VPN != state.VPNReconnecting {
		if time.Now().After(deadline) {
			t.Fatalf("state after loss = %#v", server.store.Get())
		}
		time.Sleep(5 * time.Millisecond)
	}
	second, _ := dialControl()
	defer second.Close()
	if got := server.store.Get(); got.Reconnects != 1 || got.VPN != state.VPNStarting {
		t.Fatalf("state after reconnect = %#v", got)
	}

	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err = control.Call(stopContext, paths.ControlSocket, control.Request{Command: control.CommandStop})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	stopFrame, err := protocol.ReadFrame(second)
	if err != nil || stopFrame.Type != protocol.TypeStopRequest {
		t.Fatalf("STOP_REQUEST = %#v, error = %v", stopFrame, err)
	}
	if err := protocol.WriteFrame(second, protocol.Frame{Type: protocol.TypeStopAck, Sequence: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case runError := <-runResult:
		if runError != nil {
			t.Fatal(runError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestControlLossWinsOverConcurrentDataHandshakeState(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-daemon-data-control-race-")
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
		Paths: paths, DeviceID: "secret-device-id", DeviceLabel: "device-redacted", Forwarder: forwarder,
		RelayFactory: func() (PacketRelay, error) { return newFakePacketRelay(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	dataPublishReached := make(chan struct{})
	allowDataPublish := make(chan struct{})
	controlReleaseReached := make(chan struct{})
	var dataHookOnce sync.Once
	var controlHookOnce sync.Once
	server.testBeforeDataStatePublish = func() {
		dataHookOnce.Do(func() { close(dataPublishReached) })
		<-allowDataPublish
	}
	server.testBeforeControlRelease = func() {
		controlHookOnce.Do(func() { close(controlReleaseReached) })
	}

	runResult := make(chan error, 1)
	go func() { runResult <- server.Run(context.Background()) }()
	var mapping hdc.Mapping
	select {
	case mapping = <-forwarder.added:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not start")
	}
	dial := func() net.Conn {
		connection, dialError := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)), time.Second)
		if dialError != nil {
			t.Fatal(dialError)
		}
		return connection
	}

	controlConnection := dial()
	controlHello, _ := protocol.MarshalJSONPayload(protocol.Hello{
		Role: "control", Mode: "vpn", AppVersion: "test", SupportedVersions: []int{1},
		Capabilities: []string{"control", "data"}, Message: "hello",
	})
	if err := protocol.WriteFrame(controlConnection, protocol.Frame{Type: protocol.TypeHello, Sequence: 1, Payload: controlHello}); err != nil {
		t.Fatal(err)
	}
	controlAck, err := protocol.ReadFrame(controlConnection)
	if err != nil {
		t.Fatal(err)
	}
	var acknowledgement protocol.HelloAck
	if controlAck.Type != protocol.TypeHelloAck ||
		protocol.UnmarshalJSONPayload(controlAck.Payload, &acknowledgement) != nil {
		t.Fatalf("invalid control acknowledgement: %#v", controlAck)
	}

	dataConnection := dial()
	defer dataConnection.Close()
	dataHello, _ := protocol.MarshalJSONPayload(protocol.DataHello{
		SessionToken: acknowledgement.SessionToken,
		Role:         "data",
	})
	if err := protocol.WriteFrame(dataConnection, protocol.Frame{Type: protocol.TypeDataHello, Sequence: 1, Payload: dataHello}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dataPublishReached:
	case <-time.After(3 * time.Second):
		t.Fatal("data handshake did not reach the state publication boundary")
	}
	if err := controlConnection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-controlReleaseReached:
	case <-time.After(3 * time.Second):
		t.Fatal("control loss was not observed")
	}
	close(allowDataPublish)

	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot := server.store.Get()
		if snapshot.Transport == state.TransportPortReady && snapshot.VPN == state.VPNReconnecting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control loss was overwritten by the data handshake: %#v", snapshot)
		}
		time.Sleep(5 * time.Millisecond)
	}

	stopContext, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err = control.Call(stopContext, paths.ControlSocket, control.Request{Command: control.CommandStop})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case runError := <-runResult:
		if runError != nil {
			t.Fatal(runError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
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

func TestReleaseControlClassifiesVPNDisconnect(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name          string
		initialVPN    state.VPNStatus
		initialCode   string
		wantVPN       state.VPNStatus
		wantErrorCode string
	}{
		{name: "clean stop", initialVPN: state.VPNStopped, wantVPN: state.VPNStopped},
		{
			name:          "unexpected active disconnect",
			initialVPN:    state.VPNActive,
			wantVPN:       state.VPNReconnecting,
			wantErrorCode: string(apperror.CodeAppDisconnected),
		},
		{
			name:          "existing failure",
			initialVPN:    state.VPNFailed,
			initialCode:   string(apperror.CodeVPNCreateFailed),
			wantVPN:       state.VPNReconnecting,
			wantErrorCode: string(apperror.CodeAppDisconnected),
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "hnb-release-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
			if err != nil {
				t.Fatal(err)
			}
			if err := paths.Ensure(); err != nil {
				t.Fatal(err)
			}
			connection, peer := net.Pipe()
			defer connection.Close()
			defer peer.Close()
			server := &Server{
				config: Config{
					Paths:  paths,
					Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
					Now:    time.Now,
				},
				store: state.NewStore(state.NewStarting(time.Now(), "device-redacted", DefaultDevicePort)),
			}
			server.controlConnection = connection
			server.controlMode = "vpn"
			server.store.Update(time.Now(), func(snapshot *state.Snapshot) {
				snapshot.VPN = testCase.initialVPN
				snapshot.LastErrorCode = testCase.initialCode
			})

			server.releaseControl(connection)
			got := server.store.Get()
			if got.Transport != state.TransportPortReady || got.VPN != testCase.wantVPN ||
				got.LastErrorCode != testCase.wantErrorCode {
				t.Fatalf("released state = %#v", got)
			}
		})
	}
}
