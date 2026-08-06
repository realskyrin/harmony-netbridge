// Package relay translates raw IPv4 packets into ordinary host TCP and UDP
// sockets. gVisor Netstack is deliberately hidden behind Engine so the rest of
// HarmonyNetBridge does not depend on its unstable internal API.
package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
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
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	DefaultMTU              = 1400
	DefaultVirtualDNS       = "198.18.0.1"
	defaultPacketQueueSize  = 1024
	defaultMaxTCPInFlight   = 1024
	defaultDialTimeout      = 15 * time.Second
	defaultUDPIdleTimeout   = 2 * time.Minute
	defaultDNSQueryTimeout  = 10 * time.Second
	maximumIPv4PacketLength = 65_535
)

const netstackNIC tcpip.NICID = 1

// DNSExchanger handles one complete DNS wire message. network is "udp" or
// "tcp" and controls which transport is used to the selected Mac resolver.
type DNSExchanger interface {
	Exchange(ctx context.Context, network string, query []byte) ([]byte, error)
}

// DialContextFunc opens an ordinary Mac socket. It is injectable so the relay
// can be integration-tested without using external network services.
type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Config defines the host adapters used by an Engine.
type Config struct {
	Logger         *slog.Logger
	DialContext    DialContextFunc
	DNS            DNSExchanger
	VirtualDNS     netip.Addr
	MTU            uint32
	PacketQueue    int
	MaxTCPInFlight int
	DialTimeout    time.Duration
	UDPIdleTimeout time.Duration
}

// Stats is a point-in-time, payload-free relay snapshot.
type Stats struct {
	PacketsFromDevice uint64
	BytesFromDevice   uint64
	PacketsToDevice   uint64
	BytesToDevice     uint64
	TCPFlows          uint64
	UDPFlows          uint64
	DNSQueries        uint64
}

// Engine owns one IPv4 Netstack instance and all host-side flow adapters for a
// single HNB data connection.
type Engine struct {
	stack      *stack.Stack
	link       *channel.Endpoint
	output     chan []byte
	logger     *slog.Logger
	dial       DialContextFunc
	dns        DNSExchanger
	virtualDNS tcpip.Address

	dialTimeout    time.Duration
	udpIdleTimeout time.Duration

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	wg        sync.WaitGroup

	packetsFromDevice atomic.Uint64
	bytesFromDevice   atomic.Uint64
	packetsToDevice   atomic.Uint64
	bytesToDevice     atomic.Uint64
	tcpFlows          atomic.Uint64
	udpFlows          atomic.Uint64
	dnsQueries        atomic.Uint64
}

// New creates a running relay engine. The returned Output channel contains
// complete raw IPv4 packets that must be sent back to the Harmony TUN.
func New(config Config) (*Engine, error) {
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if config.DialContext == nil {
		dialer := &net.Dialer{}
		config.DialContext = dialer.DialContext
	}
	if !config.VirtualDNS.IsValid() {
		config.VirtualDNS = netip.MustParseAddr(DefaultVirtualDNS)
	}
	if !config.VirtualDNS.Is4() {
		return nil, errors.New("relay virtual DNS address must be IPv4")
	}
	if config.MTU == 0 {
		config.MTU = DefaultMTU
	}
	if config.MTU < header.IPv4MinimumMTU || config.MTU > maximumIPv4PacketLength {
		return nil, fmt.Errorf("relay MTU %d is outside the IPv4 range", config.MTU)
	}
	if config.PacketQueue <= 0 {
		config.PacketQueue = defaultPacketQueueSize
	}
	if config.MaxTCPInFlight <= 0 {
		config.MaxTCPInFlight = defaultMaxTCPInFlight
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = defaultDialTimeout
	}
	if config.UDPIdleTimeout <= 0 {
		config.UDPIdleTimeout = defaultUDPIdleTimeout
	}

	ctx, cancel := context.WithCancel(context.Background())
	linkEndpoint := channel.New(config.PacketQueue, config.MTU, "")
	netstack := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if err := netstack.CreateNIC(netstackNIC, linkEndpoint); err != nil {
		cancel()
		netstack.Close()
		return nil, fmt.Errorf("create relay Netstack NIC: %s", err)
	}
	if err := netstack.SetPromiscuousMode(netstackNIC, true); err != nil {
		cancel()
		netstack.Close()
		return nil, fmt.Errorf("enable relay promiscuous mode: %s", err)
	}
	if err := netstack.SetSpoofing(netstackNIC, true); err != nil {
		cancel()
		netstack.Close()
		return nil, fmt.Errorf("enable relay address spoofing: %s", err)
	}
	netstack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: netstackNIC}})

	engine := &Engine{
		stack:          netstack,
		link:           linkEndpoint,
		output:         make(chan []byte, config.PacketQueue),
		logger:         config.Logger,
		dial:           config.DialContext,
		dns:            config.DNS,
		virtualDNS:     tcpip.AddrFrom4(config.VirtualDNS.As4()),
		dialTimeout:    config.DialTimeout,
		udpIdleTimeout: config.UDPIdleTimeout,
		ctx:            ctx,
		cancel:         cancel,
	}
	tcpForwarder := tcp.NewForwarder(netstack, 0, config.MaxTCPInFlight, engine.handleTCP)
	netstack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)
	udpForwarder := udp.NewForwarder(netstack, engine.handleUDP)
	netstack.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)

	engine.wg.Add(1)
	go engine.copyNetstackOutput()
	return engine, nil
}

// Output returns the single stream of packets generated for the Harmony TUN.
func (e *Engine) Output() <-chan []byte { return e.output }

// Inject passes one complete device-originated IPv4 packet into Netstack.
func (e *Engine) Inject(packet []byte) error {
	if err := validateIPv4Packet(packet); err != nil {
		return err
	}
	select {
	case <-e.ctx.Done():
		return errors.New("relay engine is closed")
	default:
	}
	totalLength := int(packet[2])<<8 | int(packet[3])
	owned := bytes.Clone(packet[:totalLength])
	packetBuffer := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(owned)})
	e.link.InjectInbound(ipv4.ProtocolNumber, packetBuffer)
	packetBuffer.DecRef()
	e.packetsFromDevice.Add(1)
	e.bytesFromDevice.Add(uint64(totalLength))
	return nil
}

// Snapshot returns counters only; it never exposes packet payload or full flow
// identifiers.
func (e *Engine) Snapshot() Stats {
	return Stats{
		PacketsFromDevice: e.packetsFromDevice.Load(),
		BytesFromDevice:   e.bytesFromDevice.Load(),
		PacketsToDevice:   e.packetsToDevice.Load(),
		BytesToDevice:     e.bytesToDevice.Load(),
		TCPFlows:          e.tcpFlows.Load(),
		UDPFlows:          e.udpFlows.Load(),
		DNSQueries:        e.dnsQueries.Load(),
	}
}

// Close cancels all active flows and releases Netstack resources. It is safe
// to call more than once.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.cancel()
		e.link.Close()
		e.stack.Close()
		e.stack.Wait()
		e.wg.Wait()
	})
	return nil
}

func (e *Engine) copyNetstackOutput() {
	defer e.wg.Done()
	defer close(e.output)
	for {
		packetBuffer := e.link.ReadContext(e.ctx)
		if packetBuffer == nil {
			return
		}
		packet := make([]byte, 0, packetBuffer.Size())
		for _, part := range packetBuffer.AsSlices() {
			packet = append(packet, part...)
		}
		packetBuffer.DecRef()
		if len(packet) == 0 || len(packet) > maximumIPv4PacketLength {
			continue
		}
		e.packetsToDevice.Add(1)
		e.bytesToDevice.Add(uint64(len(packet)))
		select {
		case e.output <- packet:
		case <-e.ctx.Done():
			return
		}
	}
}

func (e *Engine) handleTCP(request *tcp.ForwarderRequest) {
	id := request.ID()
	if id.LocalAddress == e.virtualDNS && id.LocalPort == 53 && e.dns != nil {
		e.acceptDNSTCP(request)
		return
	}

	dialContext, cancel := context.WithTimeout(e.ctx, e.dialTimeout)
	hostConnection, err := e.dial(dialContext, "tcp4", endpointAddress(id.LocalAddress, id.LocalPort))
	cancel()
	if err != nil {
		request.Complete(true)
		e.logger.Debug("TCP host dial failed")
		return
	}
	var queue waiter.Queue
	endpoint, endpointError := request.CreateEndpoint(&queue)
	if endpointError != nil {
		_ = hostConnection.Close()
		request.Complete(true)
		e.logger.Debug("TCP Netstack endpoint creation failed")
		return
	}
	request.Complete(false)
	deviceConnection := gonet.NewTCPConn(&queue, endpoint)
	e.tcpFlows.Add(1)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		bridgeTCP(e.ctx, deviceConnection, hostConnection)
	}()
}

func (e *Engine) acceptDNSTCP(request *tcp.ForwarderRequest) {
	var queue waiter.Queue
	endpoint, endpointError := request.CreateEndpoint(&queue)
	if endpointError != nil {
		request.Complete(true)
		return
	}
	request.Complete(false)
	deviceConnection := gonet.NewTCPConn(&queue, endpoint)
	e.tcpFlows.Add(1)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer deviceConnection.Close()
		e.serveDNSTCP(deviceConnection)
	}()
}

func (e *Engine) handleUDP(request *udp.ForwarderRequest) bool {
	id := request.ID()
	if id.LocalAddress == e.virtualDNS && id.LocalPort == 53 && e.dns != nil {
		var queue waiter.Queue
		endpoint, endpointError := request.CreateEndpoint(&queue)
		if endpointError != nil {
			return false
		}
		deviceConnection := gonet.NewUDPConn(&queue, endpoint)
		e.udpFlows.Add(1)
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer deviceConnection.Close()
			e.serveDNSUDP(deviceConnection)
		}()
		return true
	}

	dialContext, cancel := context.WithTimeout(e.ctx, e.dialTimeout)
	hostConnection, err := e.dial(dialContext, "udp4", endpointAddress(id.LocalAddress, id.LocalPort))
	cancel()
	if err != nil {
		e.logger.Debug("UDP host dial failed")
		return false
	}
	var queue waiter.Queue
	endpoint, endpointError := request.CreateEndpoint(&queue)
	if endpointError != nil {
		_ = hostConnection.Close()
		return false
	}
	deviceConnection := gonet.NewUDPConn(&queue, endpoint)
	e.udpFlows.Add(1)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		bridgeDatagrams(e.ctx, deviceConnection, hostConnection, e.udpIdleTimeout)
	}()
	return true
}

func (e *Engine) serveDNSUDP(device net.Conn) {
	buffer := make([]byte, maximumIPv4PacketLength)
	for {
		_ = device.SetReadDeadline(time.Now().Add(e.udpIdleTimeout))
		length, err := device.Read(buffer)
		if err != nil {
			return
		}
		queryContext, cancel := context.WithTimeout(e.ctx, defaultDNSQueryTimeout)
		response, exchangeError := e.dns.Exchange(queryContext, "udp", bytes.Clone(buffer[:length]))
		cancel()
		if exchangeError != nil {
			e.logger.Debug("DNS UDP exchange failed")
			continue
		}
		e.dnsQueries.Add(1)
		_ = device.SetWriteDeadline(time.Now().Add(defaultDNSQueryTimeout))
		if _, err := device.Write(response); err != nil {
			return
		}
	}
}

func (e *Engine) serveDNSTCP(device net.Conn) {
	for {
		_ = device.SetReadDeadline(time.Now().Add(e.udpIdleTimeout))
		query, err := readDNSFrame(device)
		if err != nil {
			return
		}
		queryContext, cancel := context.WithTimeout(e.ctx, defaultDNSQueryTimeout)
		response, exchangeError := e.dns.Exchange(queryContext, "tcp", query)
		cancel()
		if exchangeError != nil {
			e.logger.Debug("DNS TCP exchange failed")
			return
		}
		e.dnsQueries.Add(1)
		_ = device.SetWriteDeadline(time.Now().Add(defaultDNSQueryTimeout))
		if err := writeDNSFrame(device, response); err != nil {
			return
		}
	}
}

func validateIPv4Packet(packet []byte) error {
	if len(packet) < header.IPv4MinimumSize {
		return errors.New("relay packet is shorter than an IPv4 header")
	}
	if packet[0]>>4 != 4 {
		return errors.New("relay accepts IPv4 packets only")
	}
	headerLength := int(packet[0]&0x0F) * 4
	if headerLength < header.IPv4MinimumSize || headerLength > len(packet) {
		return errors.New("relay packet has an invalid IPv4 header length")
	}
	totalLength := int(packet[2])<<8 | int(packet[3])
	if totalLength < headerLength || totalLength > len(packet) || totalLength > maximumIPv4PacketLength {
		return errors.New("relay packet has an invalid IPv4 total length")
	}
	return nil
}

func endpointAddress(address tcpip.Address, port uint16) string {
	return net.JoinHostPort(address.String(), strconv.Itoa(int(port)))
}

func bridgeTCP(ctx context.Context, device *gonet.TCPConn, host net.Conn) {
	defer device.Close()
	defer host.Close()
	stopCloser := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = device.Close()
			_ = host.Close()
		case <-stopCloser:
		}
	}()

	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		_, _ = io.Copy(host, device)
		if tcpHost, ok := host.(*net.TCPConn); ok {
			_ = tcpHost.CloseWrite()
		}
		_ = device.CloseRead()
	}()
	go func() {
		defer group.Done()
		_, _ = io.Copy(device, host)
		_ = device.CloseWrite()
		if tcpHost, ok := host.(*net.TCPConn); ok {
			_ = tcpHost.CloseRead()
		}
	}()
	group.Wait()
	close(stopCloser)
}

func bridgeDatagrams(ctx context.Context, device, host net.Conn, idleTimeout time.Duration) {
	defer device.Close()
	defer host.Close()
	stopCloser := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = device.Close()
			_ = host.Close()
		case <-stopCloser:
		}
	}()

	result := make(chan struct{}, 2)
	go func() {
		copyDatagrams(device, host, idleTimeout)
		result <- struct{}{}
	}()
	go func() {
		copyDatagrams(host, device, idleTimeout)
		result <- struct{}{}
	}()
	<-result
	_ = device.Close()
	_ = host.Close()
	<-result
	close(stopCloser)
}

func copyDatagrams(source, destination net.Conn, idleTimeout time.Duration) {
	packet := make([]byte, maximumIPv4PacketLength)
	for {
		_ = source.SetReadDeadline(time.Now().Add(idleTimeout))
		length, err := source.Read(packet)
		if err != nil {
			return
		}
		_ = destination.SetWriteDeadline(time.Now().Add(idleTimeout))
		written, err := destination.Write(packet[:length])
		if err != nil || written != length {
			return
		}
	}
}

func readDNSFrame(reader io.Reader) ([]byte, error) {
	var lengthBytes [2]byte
	if _, err := io.ReadFull(reader, lengthBytes[:]); err != nil {
		return nil, err
	}
	length := int(lengthBytes[0])<<8 | int(lengthBytes[1])
	if length < 12 {
		return nil, errors.New("DNS TCP frame is shorter than a DNS header")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func writeDNSFrame(writer io.Writer, payload []byte) error {
	if len(payload) < 12 || len(payload) > 65_535 {
		return errors.New("DNS TCP response length is invalid")
	}
	header := [2]byte{byte(len(payload) >> 8), byte(len(payload))}
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}
