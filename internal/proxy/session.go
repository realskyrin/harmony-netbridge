package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/realskyrin/harmony-netbridge/internal/state"
)

const (
	DefaultListenPort = 8080
	DefaultWebPort    = 8081
	readyTimeout      = 10 * time.Second
	stopTimeout       = 3 * time.Second
	managedTCPTimeout = 90
)

// Config defines one project-owned mitmweb process.
type Config struct {
	Executable  string
	ListenPort  int
	WebPort     int
	OpenBrowser bool
	CaptureFile string
	ConfDir     string
	LogFile     string
	UpstreamURL string
	SSLInsecure bool
}

// Session owns one exact mitmweb child process and its CONNECT dialer.
type Session struct {
	config Config
	info   state.ProxySnapshot
	cmd    *exec.Cmd
	dialer *ConnectDialer
	done   chan struct{}

	mu        sync.Mutex
	waitErr   error
	closing   bool
	closeOnce sync.Once
}

// Discover finds mitmweb without relying on the daemon's working directory.
func Discover(explicit string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else if environment := strings.TrimSpace(os.Getenv("HARMONY_NETBRIDGE_MITMWEB")); environment != "" {
		candidates = append(candidates, environment)
	} else {
		candidates = append(candidates, "mitmweb", "/opt/homebrew/bin/mitmweb", "/usr/local/bin/mitmweb")
	}
	for _, candidate := range candidates {
		resolved, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		absolute, err := filepath.Abs(resolved)
		if err == nil {
			return filepath.Clean(absolute), nil
		}
	}
	return "", errors.New("mitmweb was not found; install mitmproxy with Homebrew or pass --mitmweb <path>")
}

// NewCapturePath returns a collision-resistant private capture filename. The
// file is created exclusively by Start so an existing capture is never
// overwritten.
func NewCapturePath(directory string, now time.Time) (string, error) {
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create capture filename: %w", err)
	}
	name := fmt.Sprintf("harmony-%s-%s.mitm", now.UTC().Format("20060102-150405"), hex.EncodeToString(random))
	return filepath.Join(filepath.Clean(directory), name), nil
}

// Start launches mitmweb, waits for both loopback listeners, and returns only
// after the CA material and capture file have been protected.
func Start(config Config) (*Session, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	if err := CheckLoopbackPorts(config.ListenPort, config.WebPort); err != nil {
		return nil, err
	}
	for _, directory := range []string{filepath.Dir(config.CaptureFile), config.ConfDir, filepath.Dir(config.LogFile)} {
		if err := ensurePrivateDirectory(directory); err != nil {
			return nil, err
		}
	}
	capture, err := os.OpenFile(config.CaptureFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create private capture file: %w", err)
	}
	if err := capture.Close(); err != nil {
		return nil, fmt.Errorf("close private capture file: %w", err)
	}
	// Keep only the current session's mitmweb bootstrap diagnostics. This file
	// may contain the loopback Web UI's one-time token and must not accumulate
	// old credentials across runs.
	logFile, err := os.OpenFile(config.LogFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open mitmweb log: %w", err)
	}
	if err := os.Chmod(config.LogFile, 0o600); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("protect mitmweb log: %w", err)
	}

	args := managedArguments(config)
	command := exec.Command(config.Executable, args...)
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start mitmweb: %w", err)
	}
	interceptPorts := InterceptPorts()
	dialer, err := NewConnectDialer(net.JoinHostPort("127.0.0.1", strconv.Itoa(config.ListenPort)), nil, interceptPorts)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = logFile.Close()
		return nil, err
	}
	session := &Session{
		config: config,
		info: state.ProxySnapshot{
			Enabled:        true,
			Status:         state.ProxyStarting,
			PID:            command.Process.Pid,
			Executable:     config.Executable,
			ListenPort:     config.ListenPort,
			WebPort:        config.WebPort,
			OpenBrowser:    config.OpenBrowser,
			CaptureFile:    config.CaptureFile,
			CACertFile:     filepath.Join(config.ConfDir, "mitmproxy-ca-cert.cer"),
			ConfDir:        config.ConfDir,
			UpstreamURL:    config.UpstreamURL,
			SSLInsecure:    config.SSLInsecure,
			InterceptPorts: interceptPorts,
		},
		cmd:    command,
		dialer: dialer,
		done:   make(chan struct{}),
	}
	go session.wait(logFile)
	if err := session.waitReady(); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := protectOutputs(config, session.info.CACertFile); err != nil {
		_ = session.Close()
		return nil, err
	}
	session.info.Status = state.ProxyActive
	return session, nil
}

// CheckLoopbackPorts verifies that the managed proxy can bind each requested
// port. Start repeats this check inside the daemon to narrow the race window.
func CheckLoopbackPorts(ports ...int) error {
	for _, port := range ports {
		listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return fmt.Errorf("loopback port %d is already in use", port)
		}
		if err := listener.Close(); err != nil {
			return fmt.Errorf("release loopback port %d preflight: %w", port, err)
		}
	}
	return nil
}

// Info returns process metadata without authentication data or flow contents.
func (s *Session) Info() state.ProxySnapshot {
	info := s.info
	info.InterceptPorts = append([]int(nil), info.InterceptPorts...)
	return info
}

// DialContext routes selected TCP ports through this session and delegates all
// other traffic directly to the Mac network stack.
func (s *Session) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return s.dialer.DialContext(ctx, network, address)
}

func (s *Session) Intercepts(port int) bool { return s.dialer.Intercepts(port) }
func (s *Session) Done() <-chan struct{}    { return s.done }

// Err returns nil for an intentional Close and the child failure otherwise.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil
	}
	return s.waitErr
}

// Close terminates only the exact child process started by this Session.
func (s *Session) Close() error {
	var result error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
		}
		timer := time.NewTimer(stopTimeout)
		select {
		case <-s.done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			if s.cmd.Process != nil {
				result = s.cmd.Process.Kill()
			}
			<-s.done
		}
		_ = protectOutputs(s.config, s.info.CACertFile)
	})
	return result
}

func (s *Session) wait(logFile *os.File) {
	err := s.cmd.Wait()
	_ = logFile.Close()
	s.mu.Lock()
	s.waitErr = err
	s.mu.Unlock()
	close(s.done)
}

func (s *Session) waitReady() error {
	deadline := time.Now().Add(readyTimeout)
	proxyAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.config.ListenPort))
	webAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.config.WebPort))
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			return errors.New("mitmweb exited before its loopback listeners became ready; check the mitmweb log")
		default:
		}
		if loopbackReady(proxyAddress) && loopbackReady(webAddress) {
			if _, err := os.Stat(s.info.CACertFile); err == nil {
				return nil
			}
		}
		time.Sleep(75 * time.Millisecond)
	}
	return errors.New("mitmweb did not become ready within 10 seconds; check the mitmweb log")
}

func loopbackReady(address string) bool {
	connection, err := net.DialTimeout("tcp4", address, 150*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func managedArguments(config Config) []string {
	mode := "regular"
	if config.UpstreamURL != "" {
		mode = "upstream:" + config.UpstreamURL
	}
	args := []string{
		"--mode", mode,
		"--listen-host", "127.0.0.1",
		"--listen-port", strconv.Itoa(config.ListenPort),
		"--web-host", "127.0.0.1",
		"--web-port", strconv.Itoa(config.WebPort),
		"--set", "tcp_timeout=" + strconv.Itoa(managedTCPTimeout),
	}
	if config.OpenBrowser {
		args = append(args, "--web-open-browser")
	} else {
		args = append(args, "--no-web-open-browser")
	}
	if config.SSLInsecure {
		args = append(args, "--set", "ssl_insecure=true")
	}
	return append(args,
		"--save-stream-file", config.CaptureFile,
		"--set", "confdir="+config.ConfDir,
	)
}

func validateConfig(config Config) error {
	if !filepath.IsAbs(config.Executable) || !filepath.IsAbs(config.CaptureFile) ||
		!filepath.IsAbs(config.ConfDir) || !filepath.IsAbs(config.LogFile) {
		return errors.New("mitmweb executable and owned paths must be absolute")
	}
	if err := ValidateUpstreamURL(config.UpstreamURL); err != nil {
		return err
	}
	if config.ListenPort < 1 || config.ListenPort > 65_535 || config.WebPort < 1 || config.WebPort > 65_535 {
		return errors.New("mitmweb ports must be in 1...65535")
	}
	if config.ListenPort == config.WebPort {
		return errors.New("mitmweb proxy and Web UI ports must differ")
	}
	info, err := os.Stat(config.Executable)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return errors.New("mitmweb executable is not runnable")
	}
	return nil
}

// ValidateUpstreamURL accepts mitmproxy upstream host specifications without
// credentials that would be exposed in the child process command line.
func ValidateUpstreamURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	upstream, err := url.Parse(rawURL)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Hostname() == "" {
		return errors.New("upstream must be an http:// or https:// URL with a host")
	}
	if upstream.User != nil {
		return errors.New("upstream URL must not contain credentials")
	}
	if upstream.Path != "" || upstream.RawQuery != "" || upstream.Fragment != "" || upstream.ForceQuery {
		return errors.New("upstream URL must contain only a scheme, host, and optional port")
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private proxy directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect private proxy directory: %w", err)
	}
	return nil
}

func protectOutputs(config Config, caCertificate string) error {
	for _, path := range []string{config.CaptureFile, config.LogFile} {
		if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("protect proxy output: %w", err)
		}
	}
	if err := ensurePrivateDirectory(config.ConfDir); err != nil {
		return err
	}
	entries, err := os.ReadDir(config.ConfDir)
	if err != nil {
		return fmt.Errorf("inspect proxy certificate directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			if err := os.Chmod(filepath.Join(config.ConfDir, entry.Name()), 0o600); err != nil {
				return fmt.Errorf("protect proxy certificate material: %w", err)
			}
		}
	}
	if _, err := os.Stat(caCertificate); err != nil {
		return errors.New("mitmweb did not create its public CA certificate")
	}
	return nil
}

// RecoverManaged terminates an orphan only when its live command line matches
// all project-owned metadata from the private state file.
func RecoverManaged(previous state.ProxySnapshot) error {
	if !previous.Enabled || previous.PID <= 1 {
		return nil
	}
	command, err := processCommand(previous.PID)
	if err != nil {
		return nil // The recorded process no longer exists.
	}
	required := []string{
		previous.Executable,
		"--mode " + managedMode(previous.UpstreamURL),
		"--listen-port " + strconv.Itoa(previous.ListenPort),
		"--web-port " + strconv.Itoa(previous.WebPort),
		"--save-stream-file " + previous.CaptureFile,
		"confdir=" + previous.ConfDir,
	}
	if previous.SSLInsecure {
		required = append(required, "ssl_insecure=true")
	}
	for _, marker := range required {
		if marker == "" || !strings.Contains(command, marker) {
			return errors.New("a recorded proxy process is still running but cannot be safely identified; stop it manually")
		}
	}
	process, err := os.FindProcess(previous.PID)
	if err != nil {
		return nil
	}
	_ = process.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if _, err := processCommand(previous.PID); err != nil {
			return nil
		}
		time.Sleep(75 * time.Millisecond)
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("terminate orphaned mitmweb: %w", err)
	}
	killDeadline := time.Now().Add(time.Second)
	for time.Now().Before(killDeadline) {
		if _, err := processCommand(previous.PID); err != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("orphaned mitmweb did not exit after a bounded termination")
}

func managedMode(upstreamURL string) string {
	if upstreamURL == "" {
		return "regular"
	}
	return "upstream:" + upstreamURL
}

func processCommand(pid int) (string, error) {
	command := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "command=")
	payload, err := command.Output()
	if err != nil || strings.TrimSpace(string(payload)) == "" {
		return "", errors.New("process is not running")
	}
	return strings.TrimSpace(string(payload)), nil
}
