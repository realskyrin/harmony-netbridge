package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/realskyrin/harmony-netbridge/internal/state"
)

func TestConnectDialerTunnelsSelectedPortAndPreservesBufferedBytes(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requestLine := make(chan string, 1)
	go func() {
		connection, acceptError := listener.Accept()
		if acceptError != nil {
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		line, _ := reader.ReadString('\n')
		requestLine <- strings.TrimSpace(line)
		for {
			line, _ = reader.ReadString('\n')
			if line == "\r\n" || line == "" {
				break
			}
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\nworld")
	}()

	dialer, err := NewConnectDialer(listener.Addr().String(), nil, []int{443})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, err := dialer.DialContext(ctx, "tcp4", "203.0.113.7:443")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	payload := make([]byte, 5)
	if _, err := io.ReadFull(connection, payload); err != nil {
		t.Fatal(err)
	}
	if string(payload) != "world" {
		t.Fatalf("tunnel payload = %q", payload)
	}
	if got := <-requestLine; got != "CONNECT 203.0.113.7:443 HTTP/1.1" {
		t.Fatalf("request line = %q", got)
	}
}

func TestConnectDialerRejectsNonSuccessWithoutLeakingResponse(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, acceptError := listener.Accept()
		if acceptError == nil {
			defer connection.Close()
			_, _ = io.WriteString(connection, "HTTP/1.1 407 Proxy Authentication Required\r\nX-Secret: must-not-leak\r\n\r\n")
		}
	}()
	dialer, err := NewConnectDialer(listener.Addr().String(), nil, []int{443})
	if err != nil {
		t.Fatal(err)
	}
	_, err = dialer.DialContext(context.Background(), "tcp4", "203.0.113.8:443")
	if err == nil || strings.Contains(err.Error(), "must-not-leak") || strings.Contains(err.Error(), "407") {
		t.Fatalf("safe proxy error = %v", err)
	}
}

func TestConnectDialerDelegatesUDPAndUnselectedTCP(t *testing.T) {
	t.Parallel()
	var calls []string
	direct := func(_ context.Context, network, address string) (net.Conn, error) {
		calls = append(calls, network+" "+address)
		left, right := net.Pipe()
		_ = right.Close()
		return left, nil
	}
	dialer, err := NewConnectDialer("127.0.0.1:8080", direct, []int{443})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := dialer.DialContext(context.Background(), "tcp4", "203.0.113.9:22")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	connection, err = dialer.DialContext(context.Background(), "udp4", "198.18.0.1:53")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if len(calls) != 2 || calls[0] != "tcp4 203.0.113.9:22" || calls[1] != "udp4 198.18.0.1:53" {
		t.Fatalf("direct calls = %#v", calls)
	}
}

func TestRecoverManagedRefusesUnmatchedLiveProcess(t *testing.T) {
	t.Parallel()
	err := RecoverManaged(state.ProxySnapshot{
		Enabled:     true,
		PID:         os.Getpid(),
		Executable:  "/not/the/current/process",
		ListenPort:  8080,
		WebPort:     8081,
		CaptureFile: "/tmp/not-a-capture",
		ConfDir:     "/tmp/not-a-confdir",
	})
	if err == nil {
		t.Fatal("expected safe classification failure")
	}
}

func TestNewCapturePathUsesPrivateUniqueName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 6, 12, 34, 56, 0, time.UTC)
	first, err := NewCapturePath("/tmp/captures", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCapturePath("/tmp/captures", now)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "/tmp/captures/harmony-20260806-123456-") || !strings.HasSuffix(first, ".mitm") {
		t.Fatalf("capture paths = %q, %q", first, second)
	}
}

func TestStartRejectsOccupiedLoopbackPortBeforeLaunching(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	listenPort := listener.Addr().(*net.TCPAddr).Port
	webPort := listenPort + 1
	if webPort > 65_535 {
		webPort = listenPort - 1
	}
	_, err = Start(Config{
		Executable:  executable,
		ListenPort:  listenPort,
		WebPort:     webPort,
		CaptureFile: filepath.Join(root, "capture.mitm"),
		ConfDir:     filepath.Join(root, "conf"),
		LogFile:     filepath.Join(root, "mitmweb.log"),
	})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("occupied port error = %v", err)
	}
}
