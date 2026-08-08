package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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
	added          chan hdc.Mapping
	mu             sync.Mutex
	removed        []hdc.Mapping
	removeErr      error
	mappingPresent bool
	inspectErr     error
}

type fakeAppLister struct {
	applications []hdc.InstalledApplication
	err          error
	targetID     string
}

type fakePacketRelay struct {
	injected  chan []byte
	output    chan []byte
	closeOnce sync.Once
	stats     packetrelay.Stats
}

type fakeProxySession struct {
	info       state.ProxySnapshot
	done       chan struct{}
	closeOnce  sync.Once
	mu         sync.Mutex
	closeCalls int
	waitErr    error
}

func newFakeProxySession() *fakeProxySession {
	return &fakeProxySession{
		info: state.ProxySnapshot{
			Enabled:        true,
			Status:         state.ProxyActive,
			PID:            4321,
			ListenPort:     8080,
			WebPort:        8081,
			InterceptPorts: []int{80, 443, 8080, 8443},
		},
		done:    make(chan struct{}),
		waitErr: errors.New("fixture proxy exit"),
	}
}

func (p *fakeProxySession) Info() state.ProxySnapshot { return p.info }

func (p *fakeProxySession) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, errors.New("fixture proxy dial is unused")
}

func (p *fakeProxySession) Done() <-chan struct{} { return p.done }
func (p *fakeProxySession) Err() error            { return p.waitErr }

func (p *fakeProxySession) crash() { p.closeOnce.Do(func() { close(p.done) }) }

func (p *fakeProxySession) Close() error {
	p.mu.Lock()
	p.closeCalls++
	p.mu.Unlock()
	p.crash()
	return nil
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
	f.mu.Lock()
	f.mappingPresent = true
	f.mu.Unlock()
	f.added <- mapping
	return nil
}

func (f *fakeForwarder) HasReverse(_ context.Context, _ string, _ hdc.Mapping) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mappingPresent, f.inspectErr
}

func (f *fakeForwarder) Remove(_ context.Context, _ string, mapping hdc.Mapping) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, mapping)
	if f.removeErr == nil {
		f.mappingPresent = false
	}
	return f.removeErr
}

func (l *fakeAppLister) ListInstalledApplications(_ context.Context,
	targetID string) ([]hdc.InstalledApplication, error) {
	l.targetID = targetID
	return l.applications, l.err
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

func TestServerServesOnlyManagedProxyCACertificate(t *testing.T) {
	t.Parallel()
	certificate := testCACertificate(t)
	certificatePath := filepath.Join(t.TempDir(), "mitmproxy-ca-cert.cer")
	if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	proxySession := newFakeProxySession()
	proxySession.info.CACertFile = certificatePath
	server := &Server{
		config: Config{
			DeviceLabel:      "device-redacted",
			HandshakeTimeout: time.Second,
			Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		proxySession: proxySession,
	}

	response, body := exchangeDeviceHTTP(t, server, http.MethodGet,
		"GET /mitmproxy-ca-cert.cer HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("certificate status = %d, body = %q", response.StatusCode, body)
	}
	if !bytes.Equal(body, certificate) {
		t.Fatal("certificate response did not preserve the managed CA bytes")
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/pkix-cert" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("Cache-Control = %q", cacheControl)
	}
	if disposition := response.Header.Get("Content-Disposition"); disposition != `attachment; filename="mitmproxy-ca-cert.cer"` {
		t.Fatalf("Content-Disposition = %q", disposition)
	}

	response, _ = exchangeDeviceHTTP(t, server, http.MethodGet,
		"GET /capture.mitm HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unaddressable proxy file status = %d", response.StatusCode)
	}
	response, _ = exchangeDeviceHTTP(t, server, http.MethodHead,
		"HEAD /mitmproxy-ca-cert.cer HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("HEAD response = %d Allow=%q", response.StatusCode, response.Header.Get("Allow"))
	}
}

func TestServerRefusesCertificateWithoutActiveManagedProxy(t *testing.T) {
	t.Parallel()
	server := &Server{config: Config{
		DeviceLabel:      "device-redacted",
		HandshakeTimeout: time.Second,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	response, _ := exchangeDeviceHTTP(t, server, http.MethodGet,
		"GET /mitmproxy-ca-cert.cer HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("standard daemon certificate status = %d", response.StatusCode)
	}
}

func TestServerServesInstalledApplicationsForSelectedDevice(t *testing.T) {
	t.Parallel()
	const token = "0123456789abcdef0123456789abcdef"
	controlConnection, controlPeer := net.Pipe()
	defer controlConnection.Close()
	defer controlPeer.Close()
	lister := &fakeAppLister{applications: []hdc.InstalledApplication{
		{BundleName: "com.example.browser", Label: "Browser"},
		{BundleName: "com.example.mail", Label: "Mail"},
	}}
	server := &Server{config: Config{
		DeviceID:         "selected-device",
		DeviceLabel:      "device-redacted",
		HandshakeTimeout: time.Second,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		AppLister:        lister,
	}, controlConnection: controlConnection, controlToken: token}
	response, body := exchangeDeviceHTTP(t, server, http.MethodGet,
		"GET /installed-apps.json HTTP/1.1\r\nHost: 127.0.0.1\r\n"+
			"Authorization: Bearer "+token+"\r\nConnection: close\r\n\r\n")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("installed applications status = %d, body = %q", response.StatusCode, body)
	}
	if lister.targetID != "selected-device" {
		t.Fatalf("AppLister target = %q", lister.targetID)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", contentType)
	}
	var payload struct {
		Applications []hdc.InstalledApplication `json:"applications"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload.Applications, lister.applications) {
		t.Fatalf("applications = %#v, want %#v", payload.Applications, lister.applications)
	}
}

func TestServerRefusesUnavailableInstalledApplicationList(t *testing.T) {
	t.Parallel()
	const token = "abcdef0123456789abcdef0123456789"
	controlConnection, controlPeer := net.Pipe()
	defer controlConnection.Close()
	defer controlPeer.Close()
	server := &Server{config: Config{
		DeviceID:         "selected-device",
		DeviceLabel:      "device-redacted",
		HandshakeTimeout: time.Second,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		AppLister:        &fakeAppLister{err: errors.New("fixture unavailable")},
	}, controlConnection: controlConnection, controlToken: token}
	response, _ := exchangeDeviceHTTP(t, server, http.MethodGet,
		"GET /installed-apps.json HTTP/1.1\r\nHost: 127.0.0.1\r\n"+
			"Authorization: Bearer "+token+"\r\n\r\n")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unavailable installed applications status = %d", response.StatusCode)
	}
}

func TestServerRequiresCurrentControlTokenForInstalledApplicationList(t *testing.T) {
	t.Parallel()
	server := &Server{config: Config{
		DeviceID:         "selected-device",
		DeviceLabel:      "device-redacted",
		HandshakeTimeout: time.Second,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		AppLister: &fakeAppLister{applications: []hdc.InstalledApplication{
			{BundleName: "com.example.browser", Label: "Browser"},
		}},
	}}
	response, _ := exchangeDeviceHTTP(t, server, http.MethodGet,
		"GET /installed-apps.json HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("unauthorized installed applications response = %d, challenge = %q",
			response.StatusCode, response.Header.Get("WWW-Authenticate"))
	}
}

func exchangeDeviceHTTP(t *testing.T, server *Server, method, request string) (*http.Response, []byte) {
	t.Helper()
	serverConnection, clientConnection := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.handleDevice(serverConnection)
		close(done)
	}()
	_ = clientConnection.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(clientConnection, request); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(clientConnection), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	_ = clientConnection.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("device HTTP handler did not return")
	}
	return response, body
}

func testCACertificate(t *testing.T) []byte {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "HarmonyNetBridge test CA"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(4_102_444_800, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestServerAdvertisesAndClosesManagedProxy(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-proxy-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &fakeForwarder{added: make(chan hdc.Mapping, 1)}
	proxySession := newFakeProxySession()
	server, err := New(Config{
		Paths: paths, DeviceID: "secret-device-id", DeviceLabel: "device-redacted", Forwarder: forwarder,
		ProxyFactory: func(_ state.ProxySnapshot) (ProxySession, error) { return proxySession, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() { runResult <- server.Run(runContext) }()
	var mapping hdc.Mapping
	select {
	case mapping = <-forwarder.added:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy daemon did not start")
	}
	connection, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := protocol.MarshalJSONPayload(protocol.Hello{
		Role: "control", Mode: "vpn", AppVersion: "test", SupportedVersions: []int{1},
		Capabilities: []string{"control", "data", "proxy"}, Message: "hello",
	})
	if err := protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeHello, Sequence: 1, Payload: hello}); err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.ReadFrame(connection)
	if err != nil {
		t.Fatal(err)
	}
	var acknowledgement protocol.HelloAck
	if err := protocol.UnmarshalJSONPayload(frame.Payload, &acknowledgement); err != nil ||
		!hasCapability(acknowledgement.Capabilities, "proxy") {
		t.Fatalf("proxy HELLO_ACK = %#v", acknowledgement)
	}
	if snapshot := server.store.Get(); snapshot.Proxy.Status != state.ProxyActive || snapshot.Proxy.PID != 4321 {
		t.Fatalf("proxy state = %#v", snapshot.Proxy)
	}
	_ = connection.Close()
	cancel()
	select {
	case runError := <-runResult:
		if runError != nil {
			t.Fatal(runError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy daemon did not stop")
	}
	proxySession.mu.Lock()
	closeCalls := proxySession.closeCalls
	proxySession.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("proxy Close calls = %d", closeCalls)
	}
	if _, err := os.Stat(paths.StateFile); !os.IsNotExist(err) {
		t.Fatalf("normal proxy shutdown retained state: %v", err)
	}
}

func TestServerAdvertisesManagedProxyToPhase1Control(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-proxy-phase1-")
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
		ProxyFactory: func(_ state.ProxySnapshot) (ProxySession, error) { return newFakeProxySession(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() { runResult <- server.Run(runContext) }()
	var mapping hdc.Mapping
	select {
	case mapping = <-forwarder.added:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy daemon did not start")
	}
	connection, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := protocol.MarshalJSONPayload(protocol.Hello{
		Role: "control", Mode: "phase1", AppVersion: "test", SupportedVersions: []int{1},
		Capabilities: []string{"control"}, Message: "hello",
	})
	if err := protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeHello, Sequence: 1, Payload: hello}); err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.ReadFrame(connection)
	if err != nil {
		t.Fatal(err)
	}
	var acknowledgement protocol.HelloAck
	if err := protocol.UnmarshalJSONPayload(frame.Payload, &acknowledgement); err != nil {
		t.Fatal(err)
	}
	if !hasCapability(acknowledgement.Capabilities, "proxy") {
		t.Fatalf("phase1 HELLO_ACK omitted proxy capability: %#v", acknowledgement)
	}
	_ = connection.Close()
	cancel()
	select {
	case runError := <-runResult:
		if runError != nil {
			t.Fatal(runError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy daemon did not stop")
	}
}

func TestServerRetainsFailureWhenManagedProxyExits(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-proxy-failure-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &fakeForwarder{added: make(chan hdc.Mapping, 1)}
	proxySession := newFakeProxySession()
	server, err := New(Config{
		Paths: paths, DeviceID: "secret-device-id", DeviceLabel: "device-redacted", Forwarder: forwarder,
		ProxyFactory: func(_ state.ProxySnapshot) (ProxySession, error) { return proxySession, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	runResult := make(chan error, 1)
	go func() { runResult <- server.Run(context.Background()) }()
	select {
	case <-forwarder.added:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy daemon did not start")
	}
	proxySession.crash()
	select {
	case runError := <-runResult:
		if runError == nil {
			t.Fatal("Run error = nil after proxy crash")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy crash did not stop daemon")
	}
	snapshot, err := state.ReadFile(paths.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Daemon != state.DaemonFailed || snapshot.Proxy.Status != state.ProxyFailed ||
		snapshot.Proxy.PID != 0 || snapshot.LastErrorCode != string(apperror.CodeProxyUnavailable) {
		t.Fatalf("proxy failure state = %#v", snapshot)
	}
}

func TestStandardServerRecoversPreviousProxyOrphan(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-proxy-mode-switch-")
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
	previous.Proxy = state.ProxySnapshot{Enabled: true, Status: state.ProxyActive, PID: 2468}
	if err := state.WriteFile(paths.StateFile, previous); err != nil {
		t.Fatal(err)
	}
	forwarder := &fakeForwarder{added: make(chan hdc.Mapping, 1)}
	var recovered state.ProxySnapshot
	server, err := New(Config{
		Paths: paths, DeviceID: "secret-device-id", DeviceLabel: "device-redacted", Forwarder: forwarder,
		ProxyRecovery: func(snapshot state.ProxySnapshot) error {
			recovered = snapshot
			return nil
		},
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
		t.Fatal("standard daemon did not start")
	}
	if !recovered.Enabled || recovered.PID != 2468 || server.store.Get().Proxy.Status != state.ProxyOff {
		t.Fatalf("recovered proxy = %#v; current = %#v", recovered, server.store.Get().Proxy)
	}
	cancel()
	select {
	case runError := <-runResult:
		if runError != nil {
			t.Fatal(runError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("standard daemon did not stop")
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

func TestServerRestoresReverseMappingAfterUSBReconnect(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-daemon-usb-reconnect-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &fakeForwarder{added: make(chan hdc.Mapping, 2)}
	server, err := New(Config{
		Paths: paths, DeviceID: "secret-device-id", DeviceLabel: "device-redacted", Forwarder: forwarder,
		MappingCheckInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() { runResult <- server.Run(runContext) }()
	var mapping hdc.Mapping
	select {
	case mapping = <-forwarder.added:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not create its initial reverse mapping")
	}

	forwarder.mu.Lock()
	forwarder.mappingPresent = false
	forwarder.inspectErr = errors.New("fixture device disconnected")
	forwarder.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for server.store.Get().Transport != state.TransportDeviceOffline {
		if time.Now().After(deadline) {
			t.Fatalf("mapping loss was not reflected in state: %#v", server.store.Get())
		}
		time.Sleep(5 * time.Millisecond)
	}

	forwarder.mu.Lock()
	forwarder.inspectErr = nil
	forwarder.mu.Unlock()
	select {
	case restored := <-forwarder.added:
		if restored != mapping {
			t.Fatalf("restored mapping = %#v, want %#v", restored, mapping)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not restore the reverse mapping after reconnect")
	}
	deadline = time.Now().Add(time.Second)
	for server.store.Get().Transport != state.TransportPortReady {
		if time.Now().After(deadline) {
			t.Fatalf("restored mapping was not reflected in state: %#v", server.store.Get())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case runError := <-runResult:
		if runError != nil {
			t.Fatal(runError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop")
	}
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	if len(forwarder.removed) != 1 || forwarder.removed[0] != mapping {
		t.Fatalf("removed mappings = %#v, want only %#v", forwarder.removed, mapping)
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
