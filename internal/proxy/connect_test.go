package proxy

import (
	"bufio"
	"bytes"
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
	connection, err := dialer.DialContext(ctx, "tcp4", "example.test:443")
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
	if got := <-requestLine; got != "CONNECT example.test:443 HTTP/1.1" {
		t.Fatalf("request line = %q", got)
	}
}

func TestConnectDialerRecoversTLSHostnameForIPDestination(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requestLine := make(chan string, 1)
	received := make(chan []byte, 1)
	clientHello := makeTLSClientHello("m.mobilex-static-uat.hsbc.com.hk")
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
		_, _ = io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\n")
		payload := make([]byte, len(clientHello))
		if _, err := io.ReadFull(reader, payload); err == nil {
			received <- payload
		}
	}()

	dialer, err := NewConnectDialer(listener.Addr().String(), nil, []int{443})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	connection, err := dialer.DialContext(ctx, "tcp4", "13.226.244.113:443")
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	middle := len(clientHello) / 2
	if _, err := connection.Write(clientHello[:middle]); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write(clientHello[middle:]); err != nil {
		t.Fatal(err)
	}
	if got := <-requestLine; got != "CONNECT m.mobilex-static-uat.hsbc.com.hk:443 HTTP/1.1" {
		t.Fatalf("request line = %q", got)
	}
	if got := <-received; !bytes.Equal(got, clientHello) {
		t.Fatalf("forwarded ClientHello differs: %x", got)
	}
}

func TestConnectDialerRecoversHTTPHostForIPDestination(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requestLine := make(chan string, 1)
	forwarded := make(chan string, 1)
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
		_, _ = io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\n")
		line, _ = reader.ReadString('\n')
		forwarded <- strings.TrimSpace(line)
	}()

	dialer, err := NewConnectDialer(listener.Addr().String(), nil, []int{80})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	connection, err := dialer.DialContext(ctx, "tcp4", "203.0.113.80:80")
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	request := "GET /status HTTP/1.1\r\nHost: api.example.test\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatal(err)
	}
	if got := <-requestLine; got != "CONNECT api.example.test:80 HTTP/1.1" {
		t.Fatalf("request line = %q", got)
	}
	if got := <-forwarded; got != "GET /status HTTP/1.1" {
		t.Fatalf("forwarded request line = %q", got)
	}
}

func makeTLSClientHello(serverName string) []byte {
	name := []byte(serverName)
	serverNameList := make([]byte, 0, len(name)+3)
	serverNameList = append(serverNameList, 0, byte(len(name)>>8), byte(len(name)))
	serverNameList = append(serverNameList, name...)
	serverNameExtension := []byte{0, 0, byte((len(serverNameList) + 2) >> 8), byte(len(serverNameList) + 2),
		byte(len(serverNameList) >> 8), byte(len(serverNameList))}
	serverNameExtension = append(serverNameExtension, serverNameList...)
	body := []byte{3, 3}
	body = append(body, make([]byte, 32)...)
	body = append(body, 0, 0, 2, 0x13, 0x01, 1, 0)
	body = append(body, byte(len(serverNameExtension)>>8), byte(len(serverNameExtension)))
	body = append(body, serverNameExtension...)
	handshake := []byte{1, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	handshake = append(handshake, body...)
	record := []byte{0x16, 3, 1, byte(len(handshake) >> 8), byte(len(handshake))}
	return append(record, handshake...)
}

func TestBufferedConnDelegatesHalfClose(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	underlying := &recordingHalfCloseConn{Conn: left}
	connection := newBufferedConn(underlying, bufio.NewReader(left))
	closeReader, ok := connection.(interface{ CloseRead() error })
	if !ok {
		t.Fatal("buffered connection does not expose CloseRead")
	}
	closeWriter, ok := connection.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("buffered connection does not expose CloseWrite")
	}
	if err := closeReader.CloseRead(); err != nil {
		t.Fatal(err)
	}
	if err := closeWriter.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	lingerSetter, ok := connection.(interface{ SetLinger(int) error })
	if !ok {
		t.Fatal("buffered connection does not expose SetLinger")
	}
	if err := lingerSetter.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if !underlying.readClosed || !underlying.writeClosed || underlying.linger != 0 {
		t.Fatalf("TCP delegation = read:%t write:%t linger:%d", underlying.readClosed, underlying.writeClosed,
			underlying.linger)
	}
}

type recordingHalfCloseConn struct {
	net.Conn
	readClosed  bool
	writeClosed bool
	linger      int
}

func (c *recordingHalfCloseConn) CloseRead() error {
	c.readClosed = true
	return nil
}

func (c *recordingHalfCloseConn) CloseWrite() error {
	c.writeClosed = true
	return nil
}

func (c *recordingHalfCloseConn) SetLinger(seconds int) error {
	c.linger = seconds
	return nil
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
	_, err = dialer.DialContext(context.Background(), "tcp4", "blocked.example.test:443")
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

func TestManagedArgumentsSetsTCPTimeout(t *testing.T) {
	t.Parallel()
	arguments := managedArguments(Config{
		ListenPort:  8080,
		WebPort:     8081,
		CaptureFile: "/tmp/capture.mitm",
		ConfDir:     "/tmp/mitmproxy",
	})
	if joined := strings.Join(arguments, " "); !strings.Contains(joined, "--set tcp_timeout=90") {
		t.Fatalf("managed connection lifecycle arguments = %q", joined)
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
