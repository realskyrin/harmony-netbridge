package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type resolverSourceFunc func(context.Context) ([]Resolver, error)

func (f resolverSourceFunc) Load(ctx context.Context) ([]Resolver, error) { return f(ctx) }

func TestParseScutilDNSAndLongestSuffixSelection(t *testing.T) {
	t.Parallel()
	resolvers, err := ParseScutilDNS(`DNS configuration

resolver #1
  search domain[0] : example.com
  nameserver[0] : 192.0.2.53
  port : 53
  order : 200000

resolver #2
  domain : corp.example.com
  nameserver[0] : 198.51.100.53
  order : 100200

resolver #3
  nameserver[0] : 203.0.113.53
  order : 200100
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolvers) != 3 {
		t.Fatalf("resolver count = %d, want 3", len(resolvers))
	}
	selected, err := selectResolver(resolvers, "api.corp.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := selected.Nameservers[0], netip.MustParseAddr("198.51.100.53"); got != want {
		t.Fatalf("selected nameserver = %s, want %s", got, want)
	}
	selected, err = selectResolver(resolvers, "outside.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := selected.Nameservers[0], netip.MustParseAddr("203.0.113.53"); got != want {
		t.Fatalf("default nameserver = %s, want %s", got, want)
	}
}

func TestDNSQuestionName(t *testing.T) {
	t.Parallel()
	query := makeDNSQuery(0x1234, "Api.Corp.Example")
	name, err := DNSQuestionName(query)
	if err != nil {
		t.Fatal(err)
	}
	if name != "api.corp.example" {
		t.Fatalf("question name = %q", name)
	}

	compressed := append([]byte(nil), query...)
	compressed[12] = 0xC0
	compressed[13] = 16
	name, err = DNSQuestionName(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if name != "corp.example" {
		t.Fatalf("compressed question name = %q", name)
	}
}

func TestDNSQuestionNameRejectsCompressionLoop(t *testing.T) {
	t.Parallel()
	message := make([]byte, 20)
	binary.BigEndian.PutUint16(message[4:6], 1)
	message[12] = 0xC0
	message[13] = 12
	if _, err := DNSQuestionName(message); err == nil {
		t.Fatal("DNSQuestionName accepted a compression loop")
	}
}

func TestSystemDNSRetriesTruncatedUDPOverTCP(t *testing.T) {
	t.Parallel()
	query := makeDNSQuery(0x2345, "service.corp.example")
	var udpCalls atomic.Uint64
	var tcpCalls atomic.Uint64
	dial := func(_ context.Context, network, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		switch network {
		case "udp4":
			udpCalls.Add(1)
			go func() {
				defer server.Close()
				request := make([]byte, len(query))
				if _, err := io.ReadFull(server, request); err != nil {
					return
				}
				response := bytes.Clone(request)
				response[2] |= 0x82 // response + truncated
				_, _ = server.Write(response)
			}()
		case "tcp4":
			tcpCalls.Add(1)
			go func() {
				defer server.Close()
				request, err := readDNSFrame(server)
				if err != nil {
					return
				}
				response := bytes.Clone(request)
				response[2] |= 0x80
				_ = writeDNSFrame(server, response)
			}()
		default:
			_ = client.Close()
			_ = server.Close()
			return nil, errors.New("unexpected network")
		}
		return client, nil
	}
	dns := NewSystemDNSWithSource(resolverSourceFunc(func(context.Context) ([]Resolver, error) {
		return []Resolver{{Nameservers: []netip.Addr{netip.MustParseAddr("192.0.2.53")}, Port: 53}}, nil
	}), dial, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := dns.Exchange(ctx, "udp", query)
	if err != nil {
		t.Fatal(err)
	}
	if dnsResponseTruncated(response) || udpCalls.Load() != 1 || tcpCalls.Load() != 1 {
		t.Fatalf("response = %x, UDP calls = %d, TCP calls = %d", response, udpCalls.Load(), tcpCalls.Load())
	}
}

func TestSystemDNSRefreshesResolverAfterExchangeFailure(t *testing.T) {
	t.Parallel()
	query := makeDNSQuery(0x3456, "service.corp.example")
	var loads atomic.Uint64
	source := resolverSourceFunc(func(context.Context) ([]Resolver, error) {
		if loads.Add(1) == 1 {
			return []Resolver{{Nameservers: []netip.Addr{netip.MustParseAddr("192.0.2.1")}, Port: 53}}, nil
		}
		return []Resolver{{Nameservers: []netip.Addr{netip.MustParseAddr("192.0.2.2")}, Port: 53}}, nil
	})
	dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
		if strings.Contains(address, "192.0.2.1") {
			return nil, errors.New("stale resolver")
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			request := make([]byte, len(query))
			if _, err := io.ReadFull(server, request); err != nil {
				return
			}
			response := bytes.Clone(request)
			response[2] |= 0x80
			_, _ = server.Write(response)
		}()
		return client, nil
	}
	dns := NewSystemDNSWithSource(source, dial, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := dns.Exchange(ctx, "udp", query)
	if err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 2 || !bytes.Equal(response[:2], query[:2]) {
		t.Fatalf("resolver loads = %d, response = %x", loads.Load(), response)
	}
}

func makeDNSQuery(transactionID uint16, name string) []byte {
	message := make([]byte, 12)
	binary.BigEndian.PutUint16(message[0:2], transactionID)
	binary.BigEndian.PutUint16(message[2:4], 0x0100)
	binary.BigEndian.PutUint16(message[4:6], 1)
	for _, label := range splitDomain(name) {
		message = append(message, byte(len(label)))
		message = append(message, label...)
	}
	message = append(message, 0)
	message = append(message, 0, 1, 0, 1)
	return message
}

func splitDomain(name string) []string {
	result := make([]string, 0, 4)
	start := 0
	for index := 0; index <= len(name); index++ {
		if index == len(name) || name[index] == '.' {
			result = append(result, name[start:index])
			start = index + 1
		}
	}
	return result
}
