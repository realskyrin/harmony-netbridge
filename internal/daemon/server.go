// Package daemon owns the hdc mapping, local control socket, HNB control/data
// sessions, and one packet relay for the selected Harmony device.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/realskyrin/harmony-netbridge/internal/apperror"
	"github.com/realskyrin/harmony-netbridge/internal/control"
	"github.com/realskyrin/harmony-netbridge/internal/hdc"
	"github.com/realskyrin/harmony-netbridge/internal/protocol"
	packetrelay "github.com/realskyrin/harmony-netbridge/internal/relay"
	"github.com/realskyrin/harmony-netbridge/internal/runtimepath"
	"github.com/realskyrin/harmony-netbridge/internal/state"
)

const (
	DefaultDevicePort        = 27183
	DefaultMTU               = int(packetrelay.DefaultMTU)
	MinimumMTU               = 576
	MaximumMTU               = 1500
	defaultHandshakeTimeout  = 5 * time.Second
	defaultHeartbeatInterval = 5 * time.Second
	defaultHeartbeatTimeout  = 15 * time.Second
	controlRequestTimeout    = 5 * time.Second
	controlStopAckTimeout    = 2 * time.Second
	cleanupTimeout           = 5 * time.Second
	certificateDownloadPath  = "/mitmproxy-ca-cert.cer"
	maxCertificateBytes      = 64 * 1024
	maxHTTPRequestBytes      = 8 * 1024
)

// Forwarder is the narrow hdc capability owned by the daemon.
type Forwarder interface {
	AddReverse(ctx context.Context, targetID string, mapping hdc.Mapping) error
	Remove(ctx context.Context, targetID string, mapping hdc.Mapping) error
}

// PacketRelay is the data-plane boundary owned by one HNB VPN session.
type PacketRelay interface {
	Inject(packet []byte) error
	Output() <-chan []byte
	Snapshot() packetrelay.Stats
	Close() error
}

// RelayFactory creates a fresh relay for each authenticated data connection.
type RelayFactory func() (PacketRelay, error)

// ProxySession is the narrow lifecycle and dial boundary exposed by the
// project-owned mitmweb supervisor.
type ProxySession interface {
	Info() state.ProxySnapshot
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
	Done() <-chan struct{}
	Err() error
	Close() error
}

// ProxyFactory starts a new local capture process after orphan recovery.
type ProxyFactory func(previous state.ProxySnapshot) (ProxySession, error)

// ProxyRecovery removes an exactly identified orphan even when the next daemon
// is switching back to standard forwarding mode.
type ProxyRecovery func(previous state.ProxySnapshot) error

// Config contains all external daemon dependencies.
type Config struct {
	Paths             runtimepath.Paths
	DeviceID          string
	DeviceLabel       string
	DevicePort        int
	MTU               int
	Forwarder         Forwarder
	Logger            *slog.Logger
	Now               func() time.Time
	HandshakeTimeout  time.Duration
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	RelayFactory      RelayFactory
	ProxyFactory      ProxyFactory
	ProxyRecovery     ProxyRecovery
}

// Server supervises a single selected hdc target.
type Server struct {
	config Config
	store  *state.Store

	mu                sync.Mutex
	controlConnection net.Conn
	controlWriter     *frameWriter
	controlMode       string
	controlToken      string
	controlStopAck    chan struct{}
	dataConnection    net.Conn
	dataWriter        *frameWriter
	dataRelay         PacketRelay
	reconnectPending  bool
	stopping          bool
	stopOnce          sync.Once
	stopRequested     chan struct{}
	stateMu           sync.Mutex

	controlListener net.Listener
	deviceListener  net.Listener
	mapping         hdc.Mapping
	mappingAdded    bool
	proxySession    ProxySession

	// These hooks are nil in production and let package tests pause the two
	// sides of a data-handshake/control-loss race at deterministic boundaries.
	testBeforeDataStatePublish func()
	testBeforeControlRelease   func()
}

// frameWriter serializes one HNB direction and owns its monotonically
// increasing sequence. Control and Data use separate writers.
type frameWriter struct {
	mu       sync.Mutex
	conn     net.Conn
	sequence uint32
}

func newFrameWriter(connection net.Conn, firstSequence uint32) *frameWriter {
	return &frameWriter{conn: connection, sequence: firstSequence}
}

func (w *frameWriter) Write(frameType protocol.Type, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	sequence := w.sequence
	w.sequence = nextSequence(sequence)
	_ = w.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return protocol.WriteFrame(w.conn, protocol.Frame{Type: frameType, Sequence: sequence, Payload: payload})
}

type heartbeatTracker struct {
	mu      sync.Mutex
	pending []byte
	sentAt  time.Time
}

func (h *heartbeatTracker) begin(now time.Time, timeout time.Duration) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pending) != 0 {
		return nil, now.Sub(h.sentAt) >= timeout
	}
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, uint64(now.UnixNano()))
	h.pending = payload
	h.sentAt = now
	return bytes.Clone(payload), false
}

func (h *heartbeatTracker) acknowledge(payload []byte, now time.Time) (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pending) == 0 || !bytes.Equal(payload, h.pending) {
		return 0, false
	}
	roundTrip := now.Sub(h.sentAt)
	h.pending = nil
	h.sentAt = time.Time{}
	return roundTrip, true
}

// New validates and creates a daemon server.
func New(config Config) (*Server, error) {
	if config.DeviceID == "" {
		return nil, apperror.New(apperror.CodeDeviceNotFound, "the daemon requires one selected Harmony device")
	}
	if config.DeviceLabel == "" {
		config.DeviceLabel = hdc.RedactTarget(config.DeviceID)
	}
	if config.DevicePort == 0 {
		config.DevicePort = DefaultDevicePort
	}
	if config.DevicePort < 1 || config.DevicePort > 65_535 {
		return nil, apperror.New(apperror.CodePortConflict, "the device TCP port is outside 1...65535")
	}
	if config.MTU == 0 {
		config.MTU = DefaultMTU
	}
	if config.MTU < MinimumMTU || config.MTU > MaximumMTU {
		return nil, fmt.Errorf("VPN MTU %d is outside %d...%d", config.MTU, MinimumMTU, MaximumMTU)
	}
	if config.Forwarder == nil {
		return nil, errors.New("daemon forwarder is required")
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.HandshakeTimeout <= 0 {
		config.HandshakeTimeout = defaultHandshakeTimeout
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = defaultHeartbeatInterval
	}
	if config.HeartbeatTimeout <= 0 {
		config.HeartbeatTimeout = defaultHeartbeatTimeout
	}
	if config.HeartbeatTimeout <= config.HeartbeatInterval {
		config.HeartbeatTimeout = 3 * config.HeartbeatInterval
	}
	return &Server{config: config, stopRequested: make(chan struct{})}, nil
}

// Run blocks until a signal, local stop command, or listener failure causes a
// complete cleanup. It never removes a mapping that AddReverse did not create.
func (s *Server) Run(ctx context.Context) (runError error) {
	if err := s.config.Paths.Ensure(); err != nil {
		return err
	}

	deviceListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return apperror.Wrap(apperror.CodePortConflict, "Mac loopback listener could not be created", err)
	}
	s.deviceListener = deviceListener
	hostPort := deviceListener.Addr().(*net.TCPAddr).Port
	s.mapping = hdc.Mapping{DevicePort: s.config.DevicePort, HostPort: hostPort}

	controlListener, err := listenControlSocket(s.config.Paths.ControlSocket)
	if err != nil {
		_ = deviceListener.Close()
		return err
	}
	s.controlListener = controlListener
	initialState := state.NewStarting(s.now(), s.config.DeviceLabel, s.config.DevicePort)
	previousState, _ := state.ReadFile(s.config.Paths.StateFile)
	var staleMapping *hdc.Mapping
	if previousState.Device != "" &&
		previousState.Device == s.config.DeviceLabel && previousState.DevicePort == s.config.DevicePort {
		if previousState.Daemon == state.DaemonRunning {
			initialState.Reconnects = previousState.Reconnects + 1
			initialState.Message = "Recovering HarmonyNetBridge after an unclean daemon exit"
		}
		if previousState.HostPort > 0 && previousState.HostPort <= 65_535 {
			mapping := hdc.Mapping{DevicePort: previousState.DevicePort, HostPort: previousState.HostPort}
			staleMapping = &mapping
		}
	}
	s.store = state.NewStore(initialState)
	s.store.Update(s.now(), func(snapshot *state.Snapshot) {
		snapshot.MTU = s.config.MTU
		if s.config.ProxyFactory != nil {
			snapshot.Proxy = state.ProxySnapshot{Enabled: true, Status: state.ProxyStarting}
			snapshot.Message = "Starting the local capture proxy"
		}
	})
	if err := s.persist(); err != nil {
		_ = controlListener.Close()
		_ = deviceListener.Close()
		_ = os.Remove(s.config.Paths.ControlSocket)
		return err
	}
	defer func() {
		cleanupError := s.shutdown()
		if runError == nil && cleanupError != nil {
			runError = cleanupError
		}
	}()
	if previousState.Proxy.Enabled && s.config.ProxyRecovery != nil {
		if proxyError := s.config.ProxyRecovery(previousState.Proxy); proxyError != nil {
			s.update(func(snapshot *state.Snapshot) {
				snapshot.Proxy = previousState.Proxy
				snapshot.Proxy.Status = state.ProxyFailed
			})
			s.fail(apperror.CodeProxyUnavailable, "an orphaned capture proxy could not be safely recovered")
			return apperror.Wrap(apperror.CodeProxyUnavailable,
				"an orphaned capture proxy could not be safely recovered", proxyError)
		}
	}
	if s.config.ProxyFactory != nil {
		proxySession, proxyError := s.config.ProxyFactory(previousState.Proxy)
		if proxyError != nil {
			s.update(func(snapshot *state.Snapshot) {
				snapshot.Proxy.Status = state.ProxyFailed
			})
			s.fail(apperror.CodeProxyUnavailable, "the local capture proxy could not be started")
			return apperror.Wrap(apperror.CodeProxyUnavailable, "the local capture proxy could not be started", proxyError)
		}
		s.proxySession = proxySession
		proxyInfo := proxySession.Info()
		s.update(func(snapshot *state.Snapshot) {
			snapshot.Proxy = proxyInfo
			snapshot.Message = "Local capture proxy ready; preparing the USB bridge"
		})
	}

	if staleMapping != nil {
		staleContext, cancelStale := context.WithTimeout(ctx, cleanupTimeout)
		staleError := s.config.Forwarder.Remove(staleContext, s.config.DeviceID, *staleMapping)
		cancelStale()
		if staleError != nil {
			s.config.Logger.Warn("could not remove the previously recorded hdc mapping before recovery",
				"device", s.config.DeviceLabel)
		} else {
			s.config.Logger.Info("removed the previously recorded hdc mapping before recovery",
				"device", s.config.DeviceLabel)
		}
	}
	if err := s.config.Forwarder.AddReverse(ctx, s.config.DeviceID, s.mapping); err != nil {
		s.fail(apperror.CodeRPortFailed, "hdc reverse mapping could not be created")
		return err
	}
	s.mappingAdded = true
	s.update(func(snapshot *state.Snapshot) {
		snapshot.Daemon = state.DaemonRunning
		snapshot.Transport = state.TransportPortReady
		snapshot.HostPort = hostPort
		snapshot.Message = "Waiting for HarmonyNetBridge App"
		snapshot.LastErrorCode = ""
		snapshot.LastError = ""
	})
	s.config.Logger.Info("daemon ready", "device", s.config.DeviceLabel, "device_port", s.config.DevicePort, "host_port", hostPort)

	serveErrors := make(chan error, 2)
	go s.serveControl(serveErrors)
	go s.serveDevice(serveErrors)

	var proxyDone <-chan struct{}
	if s.proxySession != nil {
		proxyDone = s.proxySession.Done()
	}
	select {
	case <-ctx.Done():
		return nil
	case <-s.stopRequested:
		return nil
	case err := <-serveErrors:
		if err == nil || s.isStopping() {
			return nil
		}
		s.fail(apperror.CodeInternal, "a daemon listener stopped unexpectedly")
		return err
	case <-proxyDone:
		if s.isStopping() {
			return nil
		}
		s.update(func(snapshot *state.Snapshot) {
			snapshot.Proxy.Status = state.ProxyFailed
			snapshot.Proxy.PID = 0
		})
		s.fail(apperror.CodeProxyUnavailable, "the local capture proxy stopped unexpectedly")
		if err := s.proxySession.Err(); err != nil {
			return err
		}
		return errors.New("local capture proxy stopped unexpectedly")
	}
}

func (s *Server) serveControl(errorsChannel chan<- error) {
	for {
		connection, err := s.controlListener.Accept()
		if err != nil {
			errorsChannel <- err
			return
		}
		go s.handleControl(connection)
	}
}

func (s *Server) handleControl(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(controlRequestTimeout))
	var request control.Request
	if err := control.Read(connection, &request); err != nil {
		_ = control.Write(connection, control.Response{OK: false, ErrorCode: apperror.CodeInternal, Error: "invalid daemon control request"})
		return
	}

	switch request.Command {
	case control.CommandStatus:
		s.refreshRelayStats()
		_ = control.Write(connection, control.Response{OK: true, State: s.store.Get()})
	case control.CommandStop:
		s.update(func(snapshot *state.Snapshot) {
			snapshot.Daemon = state.DaemonStopping
			snapshot.Message = "Stopping HarmonyNetBridge"
		})
		_ = control.Write(connection, control.Response{OK: true, State: s.store.Get()})
		s.requestStop()
	default:
		_ = control.Write(connection, control.Response{OK: false, ErrorCode: apperror.CodeInternal, Error: "unknown daemon control command"})
	}
}

func (s *Server) serveDevice(errorsChannel chan<- error) {
	for {
		connection, err := s.deviceListener.Accept()
		if err != nil {
			errorsChannel <- err
			return
		}
		go s.handleDevice(connection)
	}
}

func (s *Server) handleDevice(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(s.config.HandshakeTimeout))
	prefix := make([]byte, 4)
	if _, err := io.ReadFull(connection, prefix); err != nil {
		s.sendProtocolError(connection, 1, protocolErrorCode(err), "invalid HNB/1 handshake")
		s.config.Logger.Warn("handshake failed", "device", s.config.DeviceLabel, "error", safeProtocolError(err))
		return
	}
	reader := io.MultiReader(bytes.NewReader(prefix), connection)
	if looksLikeHTTPRequest(prefix) {
		s.handleCertificateRequest(connection, reader)
		return
	}
	frame, err := protocol.ReadFrame(reader)
	if err != nil {
		s.sendProtocolError(connection, 1, protocolErrorCode(err), "invalid HNB/1 handshake")
		s.config.Logger.Warn("handshake failed", "device", s.config.DeviceLabel, "error", safeProtocolError(err))
		return
	}
	if frame.Sequence != 1 {
		s.sendProtocolError(connection, 1, "INVALID_HANDSHAKE", "the first HNB/1 frame must use sequence 1")
		return
	}
	switch frame.Type {
	case protocol.TypeHello:
		s.handleControlConnection(connection, frame)
	case protocol.TypeDataHello:
		s.handleDataConnection(connection, frame)
	default:
		s.sendProtocolError(connection, 1, "INVALID_HANDSHAKE", "the first HNB/1 frame must be HELLO or DATA_HELLO")
	}
}

func looksLikeHTTPRequest(prefix []byte) bool {
	switch string(prefix) {
	case "GET ", "HEAD", "POST", "PUT ", "DELE", "PATC", "OPTI", "TRAC", "CONN", "PRI ":
		return true
	default:
		return false
	}
}

// handleCertificateRequest shares the existing hdc loopback mapping with HNB/1
// without exposing another device port. Only the current managed proxy's public
// CA certificate is served; private keys, capture files, and mitmweb metadata
// are never addressable through this handler.
func (s *Server) handleCertificateRequest(connection net.Conn, reader io.Reader) {
	limited := &io.LimitedReader{R: reader, N: maxHTTPRequestBytes + 1}
	request, err := http.ReadRequest(bufio.NewReader(limited))
	if err != nil {
		writeCertificateHTTPResponse(connection, http.StatusBadRequest, nil, nil)
		return
	}
	if request.Body != nil {
		_ = request.Body.Close()
	}
	if request.Method != http.MethodGet {
		writeCertificateHTTPResponse(connection, http.StatusMethodNotAllowed, nil, http.Header{"Allow": []string{http.MethodGet}})
		return
	}
	if request.URL.Path != certificateDownloadPath || request.URL.RawQuery != "" {
		writeCertificateHTTPResponse(connection, http.StatusNotFound, nil, nil)
		return
	}

	certificate, err := s.currentProxyCACertificate()
	if err != nil {
		writeCertificateHTTPResponse(connection, http.StatusServiceUnavailable, nil, nil)
		s.config.Logger.Warn("mitmproxy CA download unavailable", "device", s.config.DeviceLabel)
		return
	}
	writeCertificateHTTPResponse(connection, http.StatusOK, certificate, http.Header{
		"Content-Disposition": []string{`attachment; filename="mitmproxy-ca-cert.cer"`},
	})
	s.config.Logger.Info("served mitmproxy CA certificate", "device", s.config.DeviceLabel, "bytes", len(certificate))
}

func (s *Server) currentProxyCACertificate() ([]byte, error) {
	s.mu.Lock()
	proxySession := s.proxySession
	s.mu.Unlock()
	if proxySession == nil {
		return nil, errors.New("managed proxy is not active")
	}
	proxyInfo := proxySession.Info()
	certificatePath := filepath.Clean(proxyInfo.CACertFile)
	if !proxyInfo.Enabled || proxyInfo.Status != state.ProxyActive || proxyInfo.CACertFile == "" ||
		!filepath.IsAbs(certificatePath) {
		return nil, errors.New("managed proxy CA path is unavailable")
	}
	fileInfo, err := os.Lstat(certificatePath)
	if err != nil {
		return nil, err
	}
	if !fileInfo.Mode().IsRegular() || fileInfo.Size() <= 0 || fileInfo.Size() > maxCertificateBytes {
		return nil, errors.New("managed proxy CA is not a bounded regular file")
	}
	certificate, err := os.ReadFile(certificatePath)
	if err != nil {
		return nil, err
	}
	if len(certificate) == 0 || len(certificate) > maxCertificateBytes {
		return nil, errors.New("managed proxy CA size is invalid")
	}
	if err := validateCACertificate(certificate); err != nil {
		return nil, err
	}
	return certificate, nil
}

func validateCACertificate(payload []byte) error {
	der := payload
	if block, rest := pem.Decode(payload); block != nil {
		if block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
			return errors.New("managed proxy CA PEM is invalid")
		}
		der = block.Bytes
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return errors.New("managed proxy CA is not an X.509 certificate")
	}
	if !certificate.BasicConstraintsValid || !certificate.IsCA {
		return errors.New("managed proxy certificate is not a CA")
	}
	return nil
}

func writeCertificateHTTPResponse(connection net.Conn, statusCode int, body []byte, headers http.Header) {
	if body == nil {
		body = []byte(http.StatusText(statusCode) + "\n")
	}
	responseHeaders := make(http.Header)
	for name, values := range headers {
		responseHeaders[name] = append([]string(nil), values...)
	}
	responseHeaders.Set("Cache-Control", "no-store")
	responseHeaders.Set("Content-Type", "text/plain; charset=utf-8")
	if statusCode == http.StatusOK {
		responseHeaders.Set("Content-Type", "application/pkix-cert")
	}
	response := &http.Response{
		StatusCode:    statusCode,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        responseHeaders,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         true,
	}
	_ = response.Write(connection)
}

func (s *Server) handleControlConnection(connection net.Conn, frame protocol.Frame) {
	var hello protocol.Hello
	if err := protocol.UnmarshalJSONPayload(frame.Payload, &hello); err != nil {
		s.sendProtocolError(connection, 1, protocolErrorCode(err), "HELLO payload is invalid")
		return
	}
	if hello.Role != "control" || (hello.Mode != "phase1" && hello.Mode != "vpn") || hello.Message != "hello" {
		s.sendProtocolError(connection, 1, "INVALID_HANDSHAKE", "HELLO does not describe a supported control session")
		return
	}
	if !protocol.SupportsVersion(hello.SupportedVersions) {
		s.sendProtocolError(connection, 1, string(apperror.CodeVersionUnsupported), "the App does not support HNB/1")
		return
	}
	if !hasCapability(hello.Capabilities, "control") ||
		(hello.Mode == "vpn" && !hasCapability(hello.Capabilities, "data")) {
		s.sendProtocolError(connection, 1, "CAPABILITY_MISSING", "HELLO is missing required control or data capability")
		return
	}

	token, err := protocol.NewSessionToken()
	if err != nil {
		s.sendProtocolError(connection, 1, string(apperror.CodeInternal), "the Mac could not create a bridge session")
		return
	}
	writer := newFrameWriter(connection, 2)
	s.mu.Lock()
	if s.controlConnection != nil || s.stopping {
		s.mu.Unlock()
		s.sendProtocolError(connection, 1, "SESSION_BUSY", "another Harmony control session is already active")
		return
	}
	s.controlConnection = connection
	s.controlWriter = writer
	s.controlMode = hello.Mode
	s.controlToken = token
	isReconnect := hello.Mode == "vpn" && s.reconnectPending
	if hello.Mode == "vpn" {
		s.reconnectPending = false
	}
	stopAcknowledged := make(chan struct{})
	s.controlStopAck = stopAcknowledged
	s.mu.Unlock()
	defer s.releaseControl(connection)

	capabilities := []string{"control"}
	if hello.Mode == "vpn" {
		capabilities = append(capabilities, "data", "tcp", "udp", "dns", "heartbeat", "reconnect", "mtu")
		if s.proxySession != nil {
			capabilities = append(capabilities, "proxy")
		}
	}
	payload, err := protocol.MarshalJSONPayload(protocol.HelloAck{
		SelectedVersion: protocol.CurrentVersion,
		SessionToken:    token,
		Capabilities:    capabilities,
		MTU:             s.config.MTU,
		Message:         "world",
	})
	if err != nil {
		return
	}
	// Publish the accepted control-session state before HELLO_ACK becomes
	// observable by the App. This keeps reconnect counters and VPN state
	// consistent with a handshake the peer already considers complete.
	s.update(func(snapshot *state.Snapshot) {
		snapshot.Transport = state.TransportControlConnected
		if isReconnect {
			snapshot.Reconnects++
		}
		if hello.Mode == "vpn" {
			snapshot.VPN = state.VPNStarting
			snapshot.ControlHeartbeatAt = time.Time{}
			snapshot.ControlRTTMillis = 0
			snapshot.DataHeartbeatAt = time.Time{}
			snapshot.DataRTTMillis = 0
			snapshot.Relay = state.RelayStats{}
			snapshot.Message = "VPN control connected; waiting for native data channel"
		} else {
			snapshot.VPN = state.VPNStopped
			snapshot.Message = "Phase 1 handshake completed (hello/world)"
		}
		snapshot.LastErrorCode = ""
		snapshot.LastError = ""
	})
	_ = connection.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeHelloAck, Sequence: 1, Payload: payload}); err != nil {
		return
	}
	_ = connection.SetDeadline(time.Time{})
	s.config.Logger.Info("Harmony control handshake completed", "device", s.config.DeviceLabel,
		"mode", hello.Mode, "app_version", hello.AppVersion)
	controlHeartbeat := &heartbeatTracker{}
	if hello.Mode == "vpn" {
		go s.runHeartbeat("control", connection, writer, controlHeartbeat)
	}

	expectedSequence := uint32(2)
	for {
		frame, err := protocol.ReadFrame(connection)
		if err != nil {
			return
		}
		if frame.Sequence != expectedSequence {
			s.sendEstablishedError(writer, "INVALID_SEQUENCE", "control frame sequence is not contiguous")
			return
		}
		expectedSequence = nextSequence(expectedSequence)
		switch frame.Type {
		case protocol.TypeStopAck:
			close(stopAcknowledged)
			return
		case protocol.TypePong:
			if hello.Mode != "vpn" {
				return
			}
			s.recordHeartbeat("control", controlHeartbeat, frame.Payload)
		case protocol.TypePing:
			if hello.Mode != "vpn" || len(frame.Payload) != 8 || writer.Write(protocol.TypePong, frame.Payload) != nil {
				return
			}
		case protocol.TypeVPNStatus:
			if hello.Mode != "vpn" || !s.applyVPNStatus(connection, frame.Payload) {
				s.sendEstablishedError(writer, "INVALID_VPN_STATUS", "VPN_STATUS is invalid for this session")
				return
			}
		case protocol.TypeError:
			s.config.Logger.Warn("Harmony App reported a protocol error", "device", s.config.DeviceLabel)
			return
		default:
			s.sendEstablishedError(writer, "UNEXPECTED_FRAME", "this frame is not valid in a control session")
			return
		}
	}
}

func (s *Server) handleDataConnection(connection net.Conn, frame protocol.Frame) {
	var hello protocol.DataHello
	if err := protocol.UnmarshalJSONPayload(frame.Payload, &hello); err != nil ||
		hello.Role != "data" || !protocol.ValidSessionToken(hello.SessionToken) {
		s.sendProtocolError(connection, 1, "INVALID_DATA_HELLO", "DATA_HELLO payload is invalid")
		return
	}

	writer := newFrameWriter(connection, 2)
	s.mu.Lock()
	if s.stopping || s.controlConnection == nil || s.controlMode != "vpn" ||
		s.controlToken != hello.SessionToken || s.dataConnection != nil {
		s.mu.Unlock()
		s.sendProtocolError(connection, 1, "DATA_SESSION_REJECTED", "DATA_HELLO does not match an available VPN control session")
		return
	}
	s.dataConnection = connection
	s.dataWriter = writer
	s.mu.Unlock()

	dataRelay, err := s.newRelay()
	if err != nil {
		s.sendProtocolError(connection, 1, "RELAY_UNAVAILABLE", "the Mac packet relay could not be started")
		s.releaseData(connection, true)
		return
	}
	s.mu.Lock()
	if s.dataConnection != connection || s.controlConnection == nil || s.stopping {
		s.mu.Unlock()
		_ = dataRelay.Close()
		return
	}
	s.dataRelay = dataRelay
	// Publish the authenticated data-channel state before DATA_ACK becomes
	// observable by the App. Otherwise an immediate ACTIVE report can race with
	// this STARTING update and be overwritten, preventing data heartbeats. Keep
	// the session lock until publication finishes so a concurrent control loss
	// is guaranteed to publish RECONNECTING afterwards.
	if s.testBeforeDataStatePublish != nil {
		s.testBeforeDataStatePublish()
	}
	s.update(func(snapshot *state.Snapshot) {
		snapshot.Transport = state.TransportDataConnected
		snapshot.VPN = state.VPNStarting
		snapshot.Message = "VPN data channel authenticated; waiting for TUN"
		snapshot.LastErrorCode = ""
		snapshot.LastError = ""
	})
	s.mu.Unlock()
	defer s.releaseData(connection, true)

	if err := protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeDataAck, Sequence: 1}); err != nil {
		return
	}
	_ = connection.SetDeadline(time.Time{})
	s.config.Logger.Info("VPN data channel authenticated", "device", s.config.DeviceLabel)

	go s.writeRelayOutput(connection, writer, dataRelay)
	dataHeartbeat := &heartbeatTracker{}
	go s.runHeartbeat("data", connection, writer, dataHeartbeat)
	expectedSequence := uint32(2)
	for {
		packetFrame, readError := protocol.ReadFrame(connection)
		if readError != nil {
			return
		}
		if packetFrame.Sequence != expectedSequence {
			s.config.Logger.Warn("invalid frame on VPN data channel", "device", s.config.DeviceLabel)
			return
		}
		expectedSequence = nextSequence(expectedSequence)
		switch packetFrame.Type {
		case protocol.TypeIPPacket:
			if len(packetFrame.Payload) == 0 || dataRelay.Inject(packetFrame.Payload) != nil {
				s.config.Logger.Warn("invalid IPv4 packet from Harmony TUN", "device", s.config.DeviceLabel)
				return
			}
		case protocol.TypePong:
			s.recordHeartbeat("data", dataHeartbeat, packetFrame.Payload)
		case protocol.TypePing:
			if len(packetFrame.Payload) != 8 || writer.Write(protocol.TypePong, packetFrame.Payload) != nil {
				return
			}
		default:
			s.config.Logger.Warn("unexpected frame on VPN data channel", "device", s.config.DeviceLabel)
			return
		}
	}
}

func (s *Server) writeRelayOutput(connection net.Conn, writer *frameWriter, dataRelay PacketRelay) {
	for packet := range dataRelay.Output() {
		if err := writer.Write(protocol.TypeIPPacket, packet); err != nil {
			_ = connection.Close()
			return
		}
	}
	_ = connection.Close()
}

func (s *Server) runHeartbeat(kind string, connection net.Conn, writer *frameWriter, tracker *heartbeatTracker) {
	ticker := time.NewTicker(s.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if kind == "data" && s.store.Get().VPN != state.VPNActive {
				continue
			}
			now := s.now()
			payload, expired := tracker.begin(now, s.config.HeartbeatTimeout)
			if expired {
				s.config.Logger.Warn("HNB heartbeat timed out", "device", s.config.DeviceLabel, "channel", kind)
				_ = connection.Close()
				return
			}
			if payload == nil {
				continue
			}
			if err := writer.Write(protocol.TypePing, payload); err != nil {
				_ = connection.Close()
				return
			}
		case <-s.stopRequested:
			return
		}
	}
}

func (s *Server) recordHeartbeat(kind string, tracker *heartbeatTracker, payload []byte) {
	if len(payload) != 8 {
		return
	}
	now := s.now()
	roundTrip, ok := tracker.acknowledge(payload, now)
	if !ok {
		return
	}
	milliseconds := roundTrip.Milliseconds()
	if milliseconds < 0 {
		milliseconds = 0
	}
	s.update(func(snapshot *state.Snapshot) {
		if kind == "control" {
			snapshot.ControlHeartbeatAt = now.UTC()
			snapshot.ControlRTTMillis = milliseconds
		} else {
			snapshot.DataHeartbeatAt = now.UTC()
			snapshot.DataRTTMillis = milliseconds
		}
	})
}

func (s *Server) refreshRelayStats() {
	s.mu.Lock()
	relay := s.dataRelay
	s.mu.Unlock()
	if relay == nil {
		return
	}
	stats := relay.Snapshot()
	s.update(func(snapshot *state.Snapshot) {
		snapshot.Relay = relayStateStats(stats)
	})
}

func (s *Server) newRelay() (PacketRelay, error) {
	if s.config.RelayFactory != nil {
		return s.config.RelayFactory()
	}
	config := packetrelay.Config{
		Logger: s.config.Logger,
		DNS:    packetrelay.NewSystemDNS(s.config.Logger),
		MTU:    uint32(s.config.MTU),
	}
	if s.proxySession != nil {
		info := s.proxySession.Info()
		config.DialContext = s.proxySession.DialContext
		config.ProxyTCPPorts = append([]int(nil), info.InterceptPorts...)
		config.BlockUDP443 = true
	}
	return packetrelay.New(config)
}

func relayStateStats(stats packetrelay.Stats) state.RelayStats {
	return state.RelayStats{
		PacketsFromDevice: stats.PacketsFromDevice,
		BytesFromDevice:   stats.BytesFromDevice,
		PacketsToDevice:   stats.PacketsToDevice,
		BytesToDevice:     stats.BytesToDevice,
		TCPFlows:          stats.TCPFlows,
		UDPFlows:          stats.UDPFlows,
		DNSQueries:        stats.DNSQueries,
		ProxyTCPFlows:     stats.ProxyTCPFlows,
		BlockedQUICFlows:  stats.BlockedQUICFlows,
	}
}

func (s *Server) applyVPNStatus(connection net.Conn, payload []byte) bool {
	var report protocol.VPNStatusPayload
	if err := protocol.UnmarshalJSONPayload(payload, &report); err != nil || len(report.Message) > 512 {
		return false
	}
	s.mu.Lock()
	validConnection := s.controlConnection == connection && s.controlMode == "vpn"
	dataConnected := s.dataConnection != nil
	s.mu.Unlock()
	if !validConnection || (report.State == string(state.VPNActive) && !dataConnected) {
		return false
	}
	var vpnStatus state.VPNStatus
	var message string
	switch report.State {
	case string(state.VPNAuthRequired):
		vpnStatus, message = state.VPNAuthRequired, "HarmonyOS is waiting for VPN authorization"
	case string(state.VPNStarting):
		vpnStatus, message = state.VPNStarting, "HarmonyOS is creating the VPN tunnel"
	case string(state.VPNReconnecting):
		vpnStatus, message = state.VPNReconnecting, "HarmonyOS is rebuilding the VPN after a transport interruption"
	case string(state.VPNActive):
		vpnStatus = state.VPNActive
		if s.proxySession != nil {
			message = "IPv4 relay and HTTP(S) capture proxy are active"
		} else {
			message = "IPv4 TCP, UDP, and DNS relay is active"
		}
	case string(state.VPNStopped):
		vpnStatus, message = state.VPNStopped, "HarmonyOS VPN stopped safely"
	case string(state.VPNFailed):
		vpnStatus, message = state.VPNFailed, "HarmonyOS reported a VPN failure"
	default:
		return false
	}
	s.update(func(snapshot *state.Snapshot) {
		snapshot.VPN = vpnStatus
		snapshot.Message = message
		if vpnStatus == state.VPNFailed {
			snapshot.LastErrorCode = string(apperror.CodeVPNCreateFailed)
			snapshot.LastError = message
		} else {
			snapshot.LastErrorCode = ""
			snapshot.LastError = ""
		}
	})
	return true
}

func (s *Server) releaseControl(connection net.Conn) {
	if s.testBeforeControlRelease != nil {
		s.testBeforeControlRelease()
	}
	previous := s.store.Get()
	s.mu.Lock()
	if s.controlConnection != connection {
		s.mu.Unlock()
		return
	}
	s.controlConnection = nil
	s.controlWriter = nil
	controlMode := s.controlMode
	s.controlMode = ""
	s.controlToken = ""
	s.controlStopAck = nil
	dataConnection := s.dataConnection
	dataRelay := s.dataRelay
	s.dataConnection = nil
	s.dataRelay = nil
	stopping := s.stopping
	if controlMode == "vpn" && !stopping && previous.VPN != state.VPNStopped {
		s.reconnectPending = true
	}
	s.mu.Unlock()
	if dataConnection != nil {
		_ = dataConnection.Close()
	}
	if dataRelay != nil {
		_ = dataRelay.Close()
	}
	if !stopping {
		s.update(func(snapshot *state.Snapshot) {
			snapshot.Transport = state.TransportPortReady
			switch {
			case controlMode == "vpn" && snapshot.VPN == state.VPNStopped:
				snapshot.Message = "HarmonyOS VPN stopped; waiting for a new connection"
				snapshot.LastErrorCode = ""
				snapshot.LastError = ""
			case controlMode == "vpn":
				snapshot.VPN = state.VPNReconnecting
				snapshot.Message = "Harmony VPN control disconnected; waiting for automatic reconnect"
				snapshot.LastErrorCode = string(apperror.CodeAppDisconnected)
				snapshot.LastError = "The Harmony VPN control connection closed"
			default:
				snapshot.VPN = state.VPNStopped
				snapshot.Message = "Harmony App disconnected; waiting for a new connection"
				snapshot.LastErrorCode = string(apperror.CodeAppDisconnected)
				snapshot.LastError = "The Harmony App control connection closed"
			}
		})
		s.config.Logger.Info("Harmony control disconnected", "device", s.config.DeviceLabel)
	}
}

func (s *Server) releaseData(connection net.Conn, failed bool) {
	s.mu.Lock()
	if s.dataConnection != connection {
		s.mu.Unlock()
		return
	}
	s.dataConnection = nil
	s.dataWriter = nil
	dataRelay := s.dataRelay
	s.dataRelay = nil
	controlConnected := s.controlConnection != nil && s.controlMode == "vpn"
	stopping := s.stopping
	s.mu.Unlock()
	_ = connection.Close()
	if dataRelay != nil {
		stats := dataRelay.Snapshot()
		s.update(func(snapshot *state.Snapshot) {
			snapshot.Relay = relayStateStats(stats)
		})
		_ = dataRelay.Close()
	}
	if controlConnected && !stopping {
		s.update(func(snapshot *state.Snapshot) {
			snapshot.Transport = state.TransportControlConnected
			if failed {
				snapshot.VPN = state.VPNReconnecting
				snapshot.Message = "VPN data channel disconnected; waiting for automatic reconnect"
				snapshot.LastErrorCode = string(apperror.CodeAppDisconnected)
				snapshot.LastError = "The Harmony VPN data connection closed"
			} else {
				snapshot.VPN = state.VPNStopped
				snapshot.Message = "VPN data channel stopped"
			}
		})
	}
}

func (s *Server) sendProtocolError(connection net.Conn, sequence uint32, code, message string) {
	payload, err := protocol.MarshalJSONPayload(protocol.ErrorPayload{Code: code, Message: message, Fatal: true})
	if err != nil {
		return
	}
	_ = connection.SetWriteDeadline(time.Now().Add(time.Second))
	_ = protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeError, Sequence: sequence, Payload: payload})
}

func (s *Server) sendEstablishedError(writer *frameWriter, code, message string) {
	payload, err := protocol.MarshalJSONPayload(protocol.ErrorPayload{Code: code, Message: message, Fatal: true})
	if err == nil {
		_ = writer.Write(protocol.TypeError, payload)
	}
}

func (s *Server) requestStop() {
	s.stopOnce.Do(func() { close(s.stopRequested) })
}

func (s *Server) shutdown() error {
	s.mu.Lock()
	s.stopping = true
	controlConnection := s.controlConnection
	controlWriter := s.controlWriter
	controlStopAck := s.controlStopAck
	dataConnection := s.dataConnection
	dataRelay := s.dataRelay
	proxySession := s.proxySession
	s.controlConnection = nil
	s.controlWriter = nil
	s.controlMode = ""
	s.controlToken = ""
	s.controlStopAck = nil
	s.dataConnection = nil
	s.dataWriter = nil
	s.dataRelay = nil
	s.proxySession = nil
	s.mu.Unlock()

	if s.store != nil {
		s.update(func(snapshot *state.Snapshot) {
			if snapshot.Daemon != state.DaemonFailed {
				snapshot.Daemon = state.DaemonStopping
				snapshot.Message = "Stopping HarmonyNetBridge"
			}
		})
	}
	if controlConnection != nil && controlWriter != nil {
		payload, _ := protocol.MarshalJSONPayload(protocol.StopRequest{Reason: "user_requested"})
		_ = controlWriter.Write(protocol.TypeStopRequest, payload)
		if controlStopAck != nil {
			timer := time.NewTimer(controlStopAckTimeout)
			select {
			case <-controlStopAck:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
				s.config.Logger.Warn("Harmony App did not acknowledge STOP before the bounded shutdown deadline",
					"device", s.config.DeviceLabel)
			}
		}
	}
	if dataConnection != nil {
		_ = dataConnection.Close()
	}
	if dataRelay != nil {
		_ = dataRelay.Close()
	}
	if controlConnection != nil {
		_ = controlConnection.Close()
	}
	if s.deviceListener != nil {
		_ = s.deviceListener.Close()
	}
	if s.controlListener != nil {
		_ = s.controlListener.Close()
	}

	var cleanupError error
	if proxySession != nil {
		if proxyError := proxySession.Close(); proxyError != nil {
			cleanupError = errors.Join(cleanupError, proxyError)
			s.update(func(snapshot *state.Snapshot) {
				snapshot.Proxy.Status = state.ProxyFailed
				snapshot.Proxy.PID = 0
			})
			s.fail(apperror.CodeProxyUnavailable, "the daemon stopped, but its capture proxy could not be terminated cleanly")
		} else if s.store != nil && s.store.Get().Daemon != state.DaemonFailed {
			s.update(func(snapshot *state.Snapshot) {
				snapshot.Proxy.Status = state.ProxyOff
				snapshot.Proxy.PID = 0
			})
		}
	}
	if s.mappingAdded {
		cleanupContext, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		mappingError := s.config.Forwarder.Remove(cleanupContext, s.config.DeviceID, s.mapping)
		cancel()
		if mappingError != nil {
			cleanupError = errors.Join(cleanupError, mappingError)
			s.config.Logger.Error("could not remove owned hdc mapping", "device", s.config.DeviceLabel, "error", mappingError)
			s.fail(apperror.CodeRPortFailed, "the daemon stopped, but its owned hdc reverse mapping could not be removed")
		}
	}
	_ = os.Remove(s.config.Paths.ControlSocket)
	failed := s.store != nil && s.store.Get().Daemon == state.DaemonFailed
	if !failed {
		_ = os.Remove(s.config.Paths.StateFile)
	}
	s.config.Logger.Info("daemon stopped", "device", s.config.DeviceLabel, "cleanup_failed", cleanupError != nil)
	return cleanupError
}

func (s *Server) update(mutate func(*state.Snapshot)) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.store == nil {
		return
	}
	snapshot := s.store.Update(s.now(), mutate)
	if err := state.WriteFile(s.config.Paths.StateFile, snapshot); err != nil {
		s.config.Logger.Error("could not persist daemon state", "error", err)
	}
}

func (s *Server) fail(code apperror.Code, message string) {
	s.update(func(snapshot *state.Snapshot) {
		snapshot.Daemon = state.DaemonFailed
		snapshot.Message = message
		snapshot.LastErrorCode = string(code)
		snapshot.LastError = message
	})
}

func (s *Server) persist() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return state.WriteFile(s.config.Paths.StateFile, s.store.Get())
}

func (s *Server) now() time.Time { return s.config.Now() }

func (s *Server) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

func listenControlSocket(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, apperror.New(apperror.CodeDaemonRunning, "the daemon control path exists and is not a Unix socket")
		}
		probe, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
		if dialErr == nil {
			_ = probe.Close()
			return nil, apperror.New(apperror.CodeDaemonRunning, "another HarmonyNetBridge daemon is already running")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return nil, apperror.New(apperror.CodeDaemonRunning, "the existing daemon control socket could not be safely classified as stale")
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale daemon control socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect daemon control socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on daemon control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("protect daemon control socket: %w", err)
	}
	return listener, nil
}

func protocolErrorCode(err error) string {
	var protocolError *protocol.ProtocolError
	if errors.As(err, &protocolError) {
		return protocolError.Code
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return string(apperror.CodeHandshakeTimeout)
	}
	return "INVALID_FRAME"
}

func safeProtocolError(err error) string {
	var protocolError *protocol.ProtocolError
	if errors.As(err, &protocolError) {
		return protocolError.Error()
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return string(apperror.CodeHandshakeTimeout)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "peer closed during handshake"
	}
	return "socket read failed"
}

func nextSequence(sequence uint32) uint32 {
	if sequence == ^uint32(0) {
		return 1
	}
	return sequence + 1
}

func hasCapability(capabilities []string, expected string) bool {
	for _, capability := range capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}
