// Package proxy supervises the local mitmweb process and adapts selected TCP
// flows to its regular HTTP proxy using CONNECT.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const maxConnectResponseHeader = 32 * 1024

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
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse TCP destination: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || !d.Intercepts(port) {
		return d.direct(ctx, network, address)
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
