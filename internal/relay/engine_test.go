package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

func TestEngineRelaysTCP(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptError := listener.Accept()
		if acceptError != nil {
			serverDone <- acceptError
			return
		}
		defer connection.Close()
		_, copyError := io.Copy(connection, connection)
		serverDone <- copyError
	}()

	engine, err := New(Config{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
	}})
	if err != nil {
		t.Fatal(err)
	}
	peer := newTestPeer(t, engine)
	connection, err := peer.dialTCP(t, [4]byte{203, 0, 113, 10}, 443)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	payload := []byte("HarmonyNetBridge TCP")
	if _, err := connection.Write(payload); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response, payload) {
		t.Fatalf("TCP response = %q", response)
	}
	stats := engine.Snapshot()
	if stats.TCPFlows != 1 || stats.PacketsFromDevice == 0 || stats.PacketsToDevice == 0 {
		t.Fatalf("TCP stats = %#v", stats)
	}
}

func TestEngineRelaysUDP(t *testing.T) {
	t.Parallel()
	server, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	go func() {
		packet := make([]byte, 2048)
		length, address, readError := server.ReadFrom(packet)
		if readError == nil {
			_, _ = server.WriteTo(packet[:length], address)
		}
	}()
	engine, err := New(Config{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.LocalAddr().String())
	}})
	if err != nil {
		t.Fatal(err)
	}
	peer := newTestPeer(t, engine)
	connection, err := peer.dialUDP([4]byte{203, 0, 113, 20}, 5353)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	payload := []byte("HarmonyNetBridge UDP")
	if _, err := connection.Write(payload); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 2048)
	length, err := connection.Read(response)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response[:length], payload) {
		t.Fatalf("UDP response = %q", response[:length])
	}
	if stats := engine.Snapshot(); stats.UDPFlows != 1 {
		t.Fatalf("UDP stats = %#v", stats)
	}
}

func TestEngineRelaysDNSOverUDPAndTCP(t *testing.T) {
	t.Parallel()
	dns := &echoDNS{}
	engine, err := New(Config{DNS: dns})
	if err != nil {
		t.Fatal(err)
	}
	peer := newTestPeer(t, engine)
	query := makeDNSQuery(0x4242, "service.corp.example")

	udpConnection, err := peer.dialUDP([4]byte{198, 18, 0, 1}, 53)
	if err != nil {
		t.Fatal(err)
	}
	_ = udpConnection.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := udpConnection.Write(query); err != nil {
		t.Fatal(err)
	}
	udpResponse := make([]byte, 2048)
	udpLength, err := udpConnection.Read(udpResponse)
	_ = udpConnection.Close()
	if err != nil {
		t.Fatal(err)
	}
	assertDNSResponse(t, query, udpResponse[:udpLength])

	tcpConnection, err := peer.dialTCP(t, [4]byte{198, 18, 0, 1}, 53)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpConnection.Close()
	_ = tcpConnection.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeDNSFrame(tcpConnection, query); err != nil {
		t.Fatal(err)
	}
	tcpResponse, err := readDNSFrame(tcpConnection)
	if err != nil {
		t.Fatal(err)
	}
	assertDNSResponse(t, query, tcpResponse)
	if got := dns.calls.Load(); got != 2 {
		t.Fatalf("DNS calls = %d, want 2", got)
	}
}

func TestEngineRejectsInvalidPackets(t *testing.T) {
	t.Parallel()
	engine, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	for _, packet := range [][]byte{{}, make([]byte, 20), append([]byte{0x60}, make([]byte, 39)...)} {
		if err := engine.Inject(packet); err == nil {
			t.Fatalf("Inject accepted packet %x", packet)
		}
	}
}

type echoDNS struct{ calls atomic.Uint64 }

func (d *echoDNS) Exchange(_ context.Context, _ string, query []byte) ([]byte, error) {
	d.calls.Add(1)
	response := bytes.Clone(query)
	response[2] |= 0x80
	return response, nil
}

func assertDNSResponse(t *testing.T, query, response []byte) {
	t.Helper()
	if len(response) < 12 || !bytes.Equal(query[:2], response[:2]) || response[2]&0x80 == 0 {
		t.Fatalf("invalid DNS response: %x", response)
	}
}

type testPeer struct {
	stack  *stack.Stack
	link   *channel.Endpoint
	engine *Engine
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newTestPeer(t *testing.T, engine *Engine) *testPeer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	peerStack := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	linkEndpoint := channel.New(1024, DefaultMTU, "")
	if err := peerStack.CreateNIC(netstackNIC, linkEndpoint); err != nil {
		t.Fatal(err)
	}
	address := tcpip.AddrFrom4([4]byte{10, 0, 0, 2})
	if err := peerStack.AddProtocolAddress(netstackNIC, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   address,
			PrefixLen: 24,
		},
	}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	peerStack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: netstackNIC}})
	peer := &testPeer{stack: peerStack, link: linkEndpoint, engine: engine, cancel: cancel}
	peer.wg.Add(2)
	go peer.copyToEngine(ctx)
	go peer.copyFromEngine(ctx)
	t.Cleanup(peer.close)
	return peer
}

func (p *testPeer) copyToEngine(ctx context.Context) {
	defer p.wg.Done()
	for {
		packetBuffer := p.link.ReadContext(ctx)
		if packetBuffer == nil {
			return
		}
		packet := make([]byte, 0, packetBuffer.Size())
		for _, part := range packetBuffer.AsSlices() {
			packet = append(packet, part...)
		}
		packetBuffer.DecRef()
		if err := p.engine.Inject(packet); err != nil {
			return
		}
	}
}

func (p *testPeer) copyFromEngine(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case packet, ok := <-p.engine.Output():
			if !ok {
				return
			}
			packetBuffer := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(bytes.Clone(packet)),
			})
			p.link.InjectInbound(ipv4.ProtocolNumber, packetBuffer)
			packetBuffer.DecRef()
		case <-ctx.Done():
			return
		}
	}
}

func (p *testPeer) dialTCP(t *testing.T, address [4]byte, port uint16) (*gonet.TCPConn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return gonet.DialContextTCP(ctx, p.stack, tcpip.FullAddress{
		NIC: netstackNIC, Addr: tcpip.AddrFrom4(address), Port: port,
	}, ipv4.ProtocolNumber)
}

func (p *testPeer) dialUDP(address [4]byte, port uint16) (*gonet.UDPConn, error) {
	return gonet.DialUDP(p.stack, nil, &tcpip.FullAddress{
		NIC: netstackNIC, Addr: tcpip.AddrFrom4(address), Port: port,
	}, ipv4.ProtocolNumber)
}

func (p *testPeer) close() {
	p.cancel()
	p.link.Close()
	p.stack.Close()
	_ = p.engine.Close()
	p.stack.Wait()
	p.wg.Wait()
}

func TestDNSFrameRoundTrip(t *testing.T) {
	t.Parallel()
	query := makeDNSQuery(1, "example.test")
	var framed bytes.Buffer
	if err := writeDNSFrame(&framed, query); err != nil {
		t.Fatal(err)
	}
	if gotLength := binary.BigEndian.Uint16(framed.Bytes()[:2]); int(gotLength) != len(query) {
		t.Fatalf("framed DNS length = %d", gotLength)
	}
	decoded, err := readDNSFrame(&framed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, query) {
		t.Fatal(fmt.Errorf("DNS frame mismatch"))
	}
}
