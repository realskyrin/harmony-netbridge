// Package daemon owns the Phase 1 listener, hdc mapping, local control socket,
// and hello/world session lifecycle.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/realskyrin/harmony-netbridge/internal/apperror"
	"github.com/realskyrin/harmony-netbridge/internal/control"
	"github.com/realskyrin/harmony-netbridge/internal/hdc"
	"github.com/realskyrin/harmony-netbridge/internal/protocol"
	"github.com/realskyrin/harmony-netbridge/internal/runtimepath"
	"github.com/realskyrin/harmony-netbridge/internal/state"
)

const (
	DefaultDevicePort       = 27183
	defaultHandshakeTimeout = 5 * time.Second
	controlRequestTimeout   = 5 * time.Second
	cleanupTimeout          = 5 * time.Second
)

// Forwarder is the narrow hdc capability owned by the daemon.
type Forwarder interface {
	AddReverse(ctx context.Context, targetID string, mapping hdc.Mapping) error
	Remove(ctx context.Context, targetID string, mapping hdc.Mapping) error
}

// Config contains all external daemon dependencies.
type Config struct {
	Paths            runtimepath.Paths
	DeviceID         string
	DeviceLabel      string
	DevicePort       int
	Forwarder        Forwarder
	Logger           *slog.Logger
	Now              func() time.Time
	HandshakeTimeout time.Duration
}

// Server supervises a single selected hdc target.
type Server struct {
	config Config
	store  *state.Store

	mu                sync.Mutex
	deviceConnection  net.Conn
	handshakeComplete bool
	stopping          bool
	stopOnce          sync.Once
	stopRequested     chan struct{}
	stateMu           sync.Mutex

	controlListener net.Listener
	deviceListener  net.Listener
	mapping         hdc.Mapping
	mappingAdded    bool
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
	s.store = state.NewStore(state.NewStarting(s.now(), s.config.DeviceLabel, s.config.DevicePort))
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
	s.mu.Lock()
	if s.deviceConnection != nil || s.stopping {
		s.mu.Unlock()
		_ = connection.Close()
		return
	}
	s.deviceConnection = connection
	s.handshakeComplete = false
	s.mu.Unlock()

	connected := false
	defer func() {
		_ = connection.Close()
		s.mu.Lock()
		if s.deviceConnection == connection {
			s.deviceConnection = nil
			s.handshakeComplete = false
		}
		stopping := s.stopping
		s.mu.Unlock()
		if connected && !stopping {
			s.update(func(snapshot *state.Snapshot) {
				snapshot.Transport = state.TransportPortReady
				snapshot.Message = "Harmony App disconnected; waiting for a new connection"
				snapshot.LastErrorCode = string(apperror.CodeAppDisconnected)
				snapshot.LastError = "The Harmony App control connection closed"
			})
			s.config.Logger.Info("Harmony App disconnected", "device", s.config.DeviceLabel)
		}
	}()

	_ = connection.SetDeadline(time.Now().Add(s.config.HandshakeTimeout))
	frame, err := protocol.ReadFrame(connection)
	if err != nil {
		s.sendProtocolError(connection, 1, protocolErrorCode(err), "invalid Phase 1 handshake")
		s.config.Logger.Warn("handshake failed", "device", s.config.DeviceLabel, "error", safeProtocolError(err))
		return
	}
	if frame.Type != protocol.TypeHello || frame.Sequence != 1 {
		s.sendProtocolError(connection, 1, "INVALID_HANDSHAKE", "the first HNB/1 frame must be HELLO sequence 1")
		return
	}
	var hello protocol.Hello
	if err := protocol.UnmarshalJSONPayload(frame.Payload, &hello); err != nil {
		s.sendProtocolError(connection, 1, protocolErrorCode(err), "HELLO payload is invalid")
		return
	}
	if hello.Role != "control" || hello.Mode != "phase1" || hello.Message != "hello" {
		s.sendProtocolError(connection, 1, "INVALID_HANDSHAKE", "HELLO does not describe a Phase 1 control session")
		return
	}
	if !protocol.SupportsVersion(hello.SupportedVersions) {
		s.sendProtocolError(connection, 1, string(apperror.CodeVersionUnsupported), "the App does not support HNB/1")
		return
	}

	token, err := protocol.NewSessionToken()
	if err != nil {
		s.sendProtocolError(connection, 1, string(apperror.CodeInternal), "the Mac could not create a bridge session")
		return
	}
	payload, err := protocol.MarshalJSONPayload(protocol.HelloAck{
		SelectedVersion: protocol.CurrentVersion,
		SessionToken:    token,
		Capabilities:    []string{"control"},
		Message:         "world",
	})
	if err != nil {
		return
	}
	if err := protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeHelloAck, Sequence: 1, Payload: payload}); err != nil {
		return
	}
	_ = connection.SetDeadline(time.Time{})
	s.mu.Lock()
	if s.deviceConnection != connection || s.stopping {
		s.mu.Unlock()
		return
	}
	s.handshakeComplete = true
	s.mu.Unlock()
	connected = true
	s.update(func(snapshot *state.Snapshot) {
		snapshot.Transport = state.TransportControlConnected
		snapshot.Message = "Phase 1 handshake completed (hello/world)"
		snapshot.LastErrorCode = ""
		snapshot.LastError = ""
	})
	s.config.Logger.Info("Phase 1 handshake completed", "device", s.config.DeviceLabel, "app_version", hello.AppVersion)

	for {
		frame, err := protocol.ReadFrame(connection)
		if err != nil {
			return
		}
		switch frame.Type {
		case protocol.TypeStopAck:
			return
		case protocol.TypeError:
			s.config.Logger.Warn("Harmony App reported a protocol error", "device", s.config.DeviceLabel)
			return
		default:
			s.sendProtocolError(connection, nextSequence(frame.Sequence), "UNEXPECTED_FRAME", "this frame is not valid in a Phase 1 control session")
			return
		}
	}
}

func (s *Server) sendProtocolError(connection net.Conn, sequence uint32, code, message string) {
	payload, err := protocol.MarshalJSONPayload(protocol.ErrorPayload{Code: code, Message: message, Fatal: true})
	if err != nil {
		return
	}
	_ = protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeError, Sequence: sequence, Payload: payload})
}

func (s *Server) requestStop() {
	s.stopOnce.Do(func() { close(s.stopRequested) })
}

func (s *Server) shutdown() error {
	s.mu.Lock()
	s.stopping = true
	connection := s.deviceConnection
	handshakeComplete := s.handshakeComplete
	s.mu.Unlock()

	if s.store != nil {
		s.update(func(snapshot *state.Snapshot) {
			if snapshot.Daemon != state.DaemonFailed {
				snapshot.Daemon = state.DaemonStopping
				snapshot.Message = "Stopping HarmonyNetBridge"
			}
		})
	}
	if connection != nil && handshakeComplete {
		payload, _ := protocol.MarshalJSONPayload(protocol.StopRequest{Reason: "user_requested"})
		_ = connection.SetWriteDeadline(time.Now().Add(time.Second))
		_ = protocol.WriteFrame(connection, protocol.Frame{Type: protocol.TypeStopRequest, Sequence: 2, Payload: payload})
	}
	if connection != nil {
		_ = connection.Close()
	}
	if s.deviceListener != nil {
		_ = s.deviceListener.Close()
	}
	if s.controlListener != nil {
		_ = s.controlListener.Close()
	}

	var cleanupError error
	if s.mappingAdded {
		cleanupContext, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		cleanupError = s.config.Forwarder.Remove(cleanupContext, s.config.DeviceID, s.mapping)
		cancel()
		if cleanupError != nil {
			s.config.Logger.Error("could not remove owned hdc mapping", "device", s.config.DeviceLabel, "error", cleanupError)
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
