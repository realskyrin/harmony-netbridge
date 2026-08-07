// Package proxy supervises the local mitmweb process and adapts selected TCP
// flows to its regular HTTP proxy using CONNECT.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxConnectResponseHeader = 32 * 1024
	maxInitialClientBytes    = 64 * 1024
)

// DialContextFunc matches net.Dialer's DialContext method.
type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// defaultInterceptPorts are the conventional HTTP(S) ports handled by
// mitmweb's regular proxy mode. Other TCP ports continue to use direct Mac
// sockets so arbitrary protocols are not accidentally treated as HTTP.
var defaultInterceptPorts = []int{80, 443, 8080, 8443}

// InterceptPorts returns a copy of the ports selected by proxy mode.
func InterceptPorts() []int { return append([]int(nil), defaultInterceptPorts...) }

// ConnectDialer opens selected TCP destinations through a loopback HTTP proxy.
// UDP and non-selected TCP ports always use the underlying direct dialer.
type ConnectDialer struct {
	proxyAddress string
	direct       DialContextFunc
	ports        map[int]struct{}
}

// NewConnectDialer validates and constructs a CONNECT dialer.
func NewConnectDialer(proxyAddress string, direct DialContextFunc, ports []int) (*ConnectDialer, error) {
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		return nil, fmt.Errorf("invalid proxy address: %w", err)
	}
	if direct == nil {
		dialer := &net.Dialer{}
		direct = dialer.DialContext
	}
	selected := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		if port < 1 || port > 65_535 {
			return nil, fmt.Errorf("invalid intercept port %d", port)
		}
		selected[port] = struct{}{}
	}
	return &ConnectDialer{proxyAddress: proxyAddress, direct: direct, ports: selected}, nil
}

// Intercepts reports whether a destination port is routed through mitmweb.
func (d *ConnectDialer) Intercepts(port int) bool {
	_, ok := d.ports[port]
	return ok
}

// DialContext implements the relay dial boundary.
func (d *ConnectDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return d.direct(ctx, network, address)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse TCP destination: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || !d.Intercepts(port) {
		return d.direct(ctx, network, address)
	}
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return newHostnameSniffingConn(ctx, d.proxyAddress, address, port, d.direct), nil
	}

	connection, err := d.direct(ctx, "tcp4", d.proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("connect to local capture proxy: %w", err)
	}
	tunnel, err := performConnect(ctx, connection, address)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	return tunnel, nil
}

func performConnect(ctx context.Context, connection net.Conn, address string) (net.Conn, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(15 * time.Second))
	}
	request := "CONNECT " + address + " HTTP/1.1\r\nHost: " + address + "\r\nProxy-Connection: Keep-Alive\r\n\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		return nil, errors.New("local capture proxy did not accept the CONNECT request")
	}

	reader := bufio.NewReader(connection)
	header, err := readBoundedHeader(reader, maxConnectResponseHeader)
	if err != nil {
		return nil, err
	}
	firstLine, _, _ := strings.Cut(string(header), "\r\n")
	parts := strings.Fields(firstLine)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") || parts[1] != "200" {
		return nil, errors.New("local capture proxy rejected the CONNECT request")
	}
	_ = connection.SetDeadline(time.Time{})
	return newBufferedConn(connection, reader), nil
}

func readBoundedHeader(reader *bufio.Reader, maximum int) ([]byte, error) {
	header := make([]byte, 0, 256)
	for len(header) < maximum {
		value, err := reader.ReadByte()
		if err != nil {
			return nil, errors.New("local capture proxy closed during CONNECT")
		}
		header = append(header, value)
		if len(header) >= 4 && string(header[len(header)-4:]) == "\r\n\r\n" {
			return header, nil
		}
	}
	return nil, errors.New("local capture proxy returned an oversized CONNECT response")
}

// bufferedConn preserves any tunnel bytes that arrived in the same read as
// the CONNECT response header.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func newBufferedConn(connection net.Conn, reader *bufio.Reader) net.Conn {
	return &bufferedConn{Conn: connection, reader: reader}
}

func (c *bufferedConn) Read(payload []byte) (int, error) {
	return c.reader.Read(payload)
}

func (c *bufferedConn) CloseRead() error {
	connection, ok := c.Conn.(interface{ CloseRead() error })
	if !ok {
		return errors.New("proxy connection does not support closing its read side")
	}
	return connection.CloseRead()
}

func (c *bufferedConn) CloseWrite() error {
	connection, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return errors.New("proxy connection does not support closing its write side")
	}
	return connection.CloseWrite()
}

func (c *bufferedConn) SetLinger(seconds int) error {
	connection, ok := c.Conn.(interface{ SetLinger(int) error })
	if !ok {
		return errors.New("proxy connection does not support configuring linger")
	}
	return connection.SetLinger(seconds)
}

type sniffResult uint8

const (
	sniffNeedMore sniffResult = iota
	sniffResolved
	sniffUnavailable
)

type hostnameSniffingConn struct {
	proxyAddress    string
	destination     string
	destinationPort int
	direct          DialContextFunc
	connectDeadline time.Time

	writeMu       sync.Mutex
	mu            sync.Mutex
	ready         chan struct{}
	readyOnce     sync.Once
	connection    net.Conn
	pending       []byte
	initErr       error
	closed        bool
	readDeadline  time.Time
	writeDeadline time.Time
}

func newHostnameSniffingConn(ctx context.Context, proxyAddress, destination string, destinationPort int,
	direct DialContextFunc) net.Conn {
	deadline, _ := ctx.Deadline()
	return &hostnameSniffingConn{
		proxyAddress: proxyAddress, destination: destination, destinationPort: destinationPort,
		direct: direct, connectDeadline: deadline, ready: make(chan struct{}),
	}
}

func (c *hostnameSniffingConn) Read(payload []byte) (int, error) {
	<-c.ready
	c.mu.Lock()
	connection, err := c.connection, c.initErr
	c.mu.Unlock()
	if connection == nil {
		if err == nil {
			err = net.ErrClosed
		}
		return 0, err
	}
	return connection.Read(payload)
}

func (c *hostnameSniffingConn) Write(payload []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	if c.connection != nil {
		connection := c.connection
		c.mu.Unlock()
		return connection.Write(payload)
	}
	if c.closed || c.initErr != nil {
		err := c.initErr
		if err == nil {
			err = net.ErrClosed
		}
		c.mu.Unlock()
		return 0, err
	}
	if len(payload) > maxInitialClientBytes-len(c.pending) {
		pending := bytes.Clone(c.pending)
		c.pending = nil
		c.mu.Unlock()
		connection, err := c.establish(c.destination)
		if err != nil {
			return 0, err
		}
		if _, err := writeAllConnection(connection, pending); err != nil {
			return 0, err
		}
		return connection.Write(payload)
	}
	c.pending = append(c.pending, payload...)
	hostname, result := sniffHostname(c.pending, c.destinationPort)
	if result == sniffNeedMore {
		c.mu.Unlock()
		return len(payload), nil
	}
	target := c.destination
	if result == sniffResolved {
		target = net.JoinHostPort(hostname, strconv.Itoa(c.destinationPort))
	}
	pending := bytes.Clone(c.pending)
	previousLength := len(pending) - len(payload)
	c.pending = nil
	c.mu.Unlock()

	connection, err := c.establish(target)
	if err != nil {
		return 0, err
	}
	written, err := writeAllConnection(connection, pending)
	currentWritten := written - previousLength
	if currentWritten < 0 {
		currentWritten = 0
	}
	if currentWritten > len(payload) {
		currentWritten = len(payload)
	}
	return currentWritten, err
}

func (c *hostnameSniffingConn) establish(target string) (net.Conn, error) {
	ctx := context.Background()
	cancel := func() {}
	if c.connectDeadline.IsZero() {
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	} else {
		ctx, cancel = context.WithDeadline(ctx, c.connectDeadline)
	}
	defer cancel()
	connection, err := c.direct(ctx, "tcp4", c.proxyAddress)
	if err == nil {
		rawConnection := connection
		connection, err = performConnect(ctx, rawConnection, target)
		if err != nil {
			_ = rawConnection.Close()
		}
	}

	c.mu.Lock()
	if c.closed {
		if connection != nil {
			_ = connection.Close()
		}
		connection = nil
		err = net.ErrClosed
	}
	if err != nil {
		c.initErr = err
	} else {
		c.connection = connection
		if !c.readDeadline.IsZero() {
			_ = connection.SetReadDeadline(c.readDeadline)
		}
		if !c.writeDeadline.IsZero() {
			_ = connection.SetWriteDeadline(c.writeDeadline)
		}
	}
	c.readyOnce.Do(func() { close(c.ready) })
	c.mu.Unlock()
	return connection, err
}

func (c *hostnameSniffingConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.initErr == nil {
		c.initErr = net.ErrClosed
	}
	connection := c.connection
	c.readyOnce.Do(func() { close(c.ready) })
	c.mu.Unlock()
	if connection != nil {
		return connection.Close()
	}
	return nil
}

func (c *hostnameSniffingConn) LocalAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connection != nil {
		return c.connection.LocalAddr()
	}
	return namedTCPAddr(c.proxyAddress)
}

func (c *hostnameSniffingConn) RemoteAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connection != nil {
		return c.connection.RemoteAddr()
	}
	return namedTCPAddr(c.destination)
}

func (c *hostnameSniffingConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline, c.writeDeadline = deadline, deadline
	connection := c.connection
	c.mu.Unlock()
	if connection != nil {
		return connection.SetDeadline(deadline)
	}
	return nil
}

func (c *hostnameSniffingConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline = deadline
	connection := c.connection
	c.mu.Unlock()
	if connection != nil {
		return connection.SetReadDeadline(deadline)
	}
	return nil
}

func (c *hostnameSniffingConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadline = deadline
	connection := c.connection
	c.mu.Unlock()
	if connection != nil {
		return connection.SetWriteDeadline(deadline)
	}
	return nil
}

func (c *hostnameSniffingConn) CloseRead() error {
	c.mu.Lock()
	connection := c.connection
	c.mu.Unlock()
	if connection == nil {
		return c.Close()
	}
	if closeReader, ok := connection.(interface{ CloseRead() error }); ok {
		return closeReader.CloseRead()
	}
	return connection.Close()
}

func (c *hostnameSniffingConn) CloseWrite() error {
	c.mu.Lock()
	connection := c.connection
	c.mu.Unlock()
	if connection == nil {
		return c.Close()
	}
	if closeWriter, ok := connection.(interface{ CloseWrite() error }); ok {
		return closeWriter.CloseWrite()
	}
	return connection.Close()
}

func (c *hostnameSniffingConn) SetLinger(seconds int) error {
	c.mu.Lock()
	connection := c.connection
	c.mu.Unlock()
	if lingerSetter, ok := connection.(interface{ SetLinger(int) error }); ok {
		return lingerSetter.SetLinger(seconds)
	}
	return nil
}

type namedTCPAddr string

func (a namedTCPAddr) Network() string { return "tcp" }
func (a namedTCPAddr) String() string  { return string(a) }

func writeAllConnection(connection net.Conn, payload []byte) (int, error) {
	total := 0
	for len(payload) > 0 {
		written, err := connection.Write(payload)
		total += written
		payload = payload[written:]
		if err != nil {
			return total, err
		}
		if written == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func sniffHostname(payload []byte, destinationPort int) (string, sniffResult) {
	if len(payload) == 0 {
		return "", sniffNeedMore
	}
	if payload[0] == 0x16 {
		return sniffTLSClientHello(payload)
	}
	if destinationPort == 443 || destinationPort == 8443 {
		return "", sniffUnavailable
	}
	return sniffHTTPHost(payload)
}

func sniffHTTPHost(payload []byte) (string, sniffResult) {
	if !bytes.Contains(payload, []byte("\r\n\r\n")) {
		return "", sniffNeedMore
	}
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(payload)))
	if err != nil {
		return "", sniffUnavailable
	}
	if request.Body != nil {
		_ = request.Body.Close()
	}
	host := request.Host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if !validProxyHostname(host) {
		return "", sniffUnavailable
	}
	return strings.ToLower(host), sniffResolved
}

func sniffTLSClientHello(payload []byte) (string, sniffResult) {
	handshake := make([]byte, 0, len(payload))
	for offset := 0; ; {
		if len(payload)-offset < 5 {
			return "", sniffNeedMore
		}
		if payload[offset] != 0x16 {
			return "", sniffUnavailable
		}
		recordLength := int(payload[offset+3])<<8 | int(payload[offset+4])
		if recordLength == 0 || recordLength > maxInitialClientBytes || len(handshake)+recordLength > maxInitialClientBytes {
			return "", sniffUnavailable
		}
		if len(payload)-offset-5 < recordLength {
			return "", sniffNeedMore
		}
		handshake = append(handshake, payload[offset+5:offset+5+recordLength]...)
		if len(handshake) >= 4 {
			if handshake[0] != 0x01 {
				return "", sniffUnavailable
			}
			handshakeLength := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
			if handshakeLength > maxInitialClientBytes-4 {
				return "", sniffUnavailable
			}
			if len(handshake) >= 4+handshakeLength {
				host, ok := clientHelloServerName(handshake[4 : 4+handshakeLength])
				if !ok {
					return "", sniffUnavailable
				}
				return host, sniffResolved
			}
		}
		offset += 5 + recordLength
		if offset == len(payload) {
			return "", sniffNeedMore
		}
	}
}

func clientHelloServerName(hello []byte) (string, bool) {
	if len(hello) < 35 {
		return "", false
	}
	offset := 34
	sessionLength := int(hello[offset])
	offset++
	if offset+sessionLength+2 > len(hello) {
		return "", false
	}
	offset += sessionLength
	cipherLength := int(hello[offset])<<8 | int(hello[offset+1])
	offset += 2
	if cipherLength == 0 || offset+cipherLength+1 > len(hello) {
		return "", false
	}
	offset += cipherLength
	compressionLength := int(hello[offset])
	offset++
	if offset+compressionLength+2 > len(hello) {
		return "", false
	}
	offset += compressionLength
	extensionsLength := int(hello[offset])<<8 | int(hello[offset+1])
	offset += 2
	if extensionsLength > len(hello)-offset {
		return "", false
	}
	end := offset + extensionsLength
	for offset+4 <= end {
		extensionType := int(hello[offset])<<8 | int(hello[offset+1])
		extensionLength := int(hello[offset+2])<<8 | int(hello[offset+3])
		offset += 4
		if extensionLength > end-offset {
			return "", false
		}
		if extensionType == 0 {
			return serverNameFromExtension(hello[offset : offset+extensionLength])
		}
		offset += extensionLength
	}
	return "", false
}

func serverNameFromExtension(extension []byte) (string, bool) {
	if len(extension) < 2 {
		return "", false
	}
	listLength := int(extension[0])<<8 | int(extension[1])
	if listLength > len(extension)-2 {
		return "", false
	}
	for offset, end := 2, 2+listLength; offset+3 <= end; {
		nameType := extension[offset]
		nameLength := int(extension[offset+1])<<8 | int(extension[offset+2])
		offset += 3
		if nameLength > end-offset {
			return "", false
		}
		if nameType == 0 {
			host := string(extension[offset : offset+nameLength])
			if validProxyHostname(host) {
				return strings.ToLower(host), true
			}
			return "", false
		}
		offset += nameLength
	}
	return "", false
}

func validProxyHostname(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}
