package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultDNSPort        = 53
	defaultResolverReload = 30 * time.Second
	maximumDNSNameJumps   = 32
)

// Resolver describes one macOS resolver block. Domains are match suffixes;
// an empty Domains slice is a default resolver.
type Resolver struct {
	Domains     []string
	Nameservers []netip.Addr
	Port        uint16
	Order       int
}

// ResolverSource loads the current macOS resolver set.
type ResolverSource interface {
	Load(ctx context.Context) ([]Resolver, error)
}

// SystemDNS forwards DNS wire messages to the current macOS resolvers. It
// performs longest-suffix resolver selection and never silently substitutes a
// public resolver.
type SystemDNS struct {
	source         ResolverSource
	logger         *slog.Logger
	reloadInterval time.Duration
	dial           DialContextFunc

	mu        sync.Mutex
	loadedAt  time.Time
	resolvers []Resolver
}

// NewSystemDNS creates the production DNS adapter backed by `scutil --dns`.
func NewSystemDNS(logger *slog.Logger) *SystemDNS {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	dialer := &net.Dialer{}
	return &SystemDNS{
		source:         ScutilResolverSource{},
		logger:         logger,
		reloadInterval: defaultResolverReload,
		dial:           dialer.DialContext,
	}
}

// NewSystemDNSWithSource exposes deterministic resolver injection for tests.
func NewSystemDNSWithSource(source ResolverSource, dial DialContextFunc, reload time.Duration) *SystemDNS {
	if reload <= 0 {
		reload = defaultResolverReload
	}
	if dial == nil {
		dialer := &net.Dialer{}
		dial = dialer.DialContext
	}
	return &SystemDNS{
		source:         source,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		reloadInterval: reload,
		dial:           dial,
	}
}

// Exchange implements DNSExchanger.
func (d *SystemDNS) Exchange(ctx context.Context, network string, query []byte) ([]byte, error) {
	if network != "udp" && network != "tcp" {
		return nil, fmt.Errorf("unsupported DNS transport %q", network)
	}
	if len(query) < 12 {
		return nil, errors.New("DNS query is shorter than its header")
	}
	name, err := DNSQuestionName(query)
	if err != nil {
		return nil, err
	}
	resolvers, err := d.currentResolvers(ctx)
	if err != nil {
		return nil, err
	}
	response, err := d.exchangeSelected(ctx, network, name, query, resolvers)
	if err == nil || ctx.Err() != nil {
		return response, err
	}

	// A corporate VPN or Wi-Fi transition can replace split-DNS resolvers before
	// the normal refresh interval. Invalidate once after an exchange failure and
	// retry against a fresh scutil snapshot; the last good snapshot remains the
	// fallback if scutil itself is temporarily unavailable.
	d.invalidateResolvers()
	refreshed, refreshError := d.currentResolvers(ctx)
	if refreshError != nil {
		return nil, err
	}
	return d.exchangeSelected(ctx, network, name, query, refreshed)
}

func (d *SystemDNS) exchangeSelected(ctx context.Context, network, name string, query []byte, resolvers []Resolver) ([]byte, error) {
	resolver, err := selectResolver(resolvers, name)
	if err != nil {
		return nil, err
	}
	var lastError error
	for _, nameserver := range resolver.Nameservers {
		if !nameserver.Is4() {
			continue
		}
		response, exchangeError := d.exchangeOne(ctx, network, nameserver, resolver.Port, query)
		if exchangeError == nil {
			if network == "udp" && dnsResponseTruncated(response) {
				response, exchangeError = d.exchangeOne(ctx, "tcp", nameserver, resolver.Port, query)
			}
		}
		if exchangeError == nil {
			return response, nil
		}
		lastError = exchangeError
	}
	if lastError == nil {
		lastError = errors.New("selected macOS resolver has no IPv4 nameserver")
	}
	return nil, fmt.Errorf("all selected macOS DNS resolvers failed: %w", lastError)
}

func (d *SystemDNS) invalidateResolvers() {
	d.mu.Lock()
	d.loadedAt = time.Time{}
	d.mu.Unlock()
}

func dnsResponseTruncated(response []byte) bool {
	return len(response) >= 4 && response[2]&0x02 != 0
}

func (d *SystemDNS) currentResolvers(ctx context.Context) ([]Resolver, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.resolvers) > 0 && time.Since(d.loadedAt) < d.reloadInterval {
		return cloneResolvers(d.resolvers), nil
	}
	if d.source == nil {
		return nil, errors.New("macOS resolver source is unavailable")
	}
	resolvers, err := d.source.Load(ctx)
	if err != nil {
		if len(d.resolvers) > 0 {
			d.logger.Warn("could not refresh macOS DNS resolvers; using the last successful snapshot")
			return cloneResolvers(d.resolvers), nil
		}
		return nil, fmt.Errorf("load macOS DNS resolvers: %w", err)
	}
	if len(resolvers) == 0 {
		return nil, errors.New("macOS reported no usable IPv4 DNS resolver")
	}
	d.resolvers = cloneResolvers(resolvers)
	d.loadedAt = time.Now()
	return cloneResolvers(d.resolvers), nil
}

func (d *SystemDNS) exchangeOne(ctx context.Context, network string, server netip.Addr, port uint16, query []byte) ([]byte, error) {
	connection, err := d.dial(ctx, network+"4", net.JoinHostPort(server.String(), strconv.Itoa(int(port))))
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if network == "tcp" {
		if err := writeDNSFrame(connection, query); err != nil {
			return nil, err
		}
		response, err := readDNSFrame(connection)
		if err != nil {
			return nil, err
		}
		return validateDNSResponse(query, response)
	}
	if written, err := connection.Write(query); err != nil || written != len(query) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return nil, err
	}
	response := make([]byte, 65_535)
	length, err := connection.Read(response)
	if err != nil {
		return nil, err
	}
	return validateDNSResponse(query, response[:length])
}

func validateDNSResponse(query, response []byte) ([]byte, error) {
	if len(response) < 12 {
		return nil, errors.New("DNS response is shorter than its header")
	}
	if !bytes.Equal(query[:2], response[:2]) {
		return nil, errors.New("DNS transaction ID does not match the query")
	}
	if response[2]&0x80 == 0 {
		return nil, errors.New("DNS message from resolver is not a response")
	}
	return bytes.Clone(response), nil
}

// DNSQuestionName returns a normalized first-question name from a DNS wire
// message, with bounded compression-pointer handling.
func DNSQuestionName(message []byte) (string, error) {
	if len(message) < 12 {
		return "", errors.New("DNS message is shorter than its header")
	}
	if binary.BigEndian.Uint16(message[4:6]) == 0 {
		return "", errors.New("DNS message has no question")
	}
	labels := make([]string, 0, 8)
	offset := 12
	jumps := 0
	visited := make(map[int]struct{})
	for {
		if offset >= len(message) {
			return "", errors.New("DNS question name is truncated")
		}
		length := int(message[offset])
		switch {
		case length == 0:
			if len(labels) == 0 {
				return ".", nil
			}
			return strings.ToLower(strings.Join(labels, ".")), nil
		case length&0xC0 == 0xC0:
			if offset+1 >= len(message) {
				return "", errors.New("DNS compression pointer is truncated")
			}
			pointer := (length&0x3F)<<8 | int(message[offset+1])
			if pointer >= len(message) {
				return "", errors.New("DNS compression pointer is outside the message")
			}
			if _, exists := visited[pointer]; exists || jumps >= maximumDNSNameJumps {
				return "", errors.New("DNS compression pointer loop detected")
			}
			visited[pointer] = struct{}{}
			offset = pointer
			jumps++
		case length&0xC0 != 0:
			return "", errors.New("DNS question contains an invalid label type")
		case length > 63:
			return "", errors.New("DNS label exceeds 63 bytes")
		default:
			if offset+1+length > len(message) {
				return "", errors.New("DNS label is truncated")
			}
			label := message[offset+1 : offset+1+length]
			for _, character := range label {
				if character < 0x21 || character > 0x7E {
					return "", errors.New("DNS label contains an unsupported character")
				}
			}
			labels = append(labels, string(label))
			offset += 1 + length
		}
	}
}

func selectResolver(resolvers []Resolver, questionName string) (Resolver, error) {
	questionName = normalizeDomain(questionName)
	bestIndex := -1
	bestSuffixLength := -1
	bestOrder := int(^uint(0) >> 1)
	for index, resolver := range resolvers {
		if len(resolver.Nameservers) == 0 {
			continue
		}
		if len(resolver.Domains) == 0 {
			if bestIndex == -1 || (bestSuffixLength == 0 && resolver.Order < bestOrder) {
				bestIndex = index
				bestSuffixLength = 0
				bestOrder = resolver.Order
			}
			continue
		}
		for _, candidate := range resolver.Domains {
			candidate = normalizeDomain(candidate)
			if candidate == "" || !domainMatches(questionName, candidate) {
				continue
			}
			if len(candidate) > bestSuffixLength || (len(candidate) == bestSuffixLength && resolver.Order < bestOrder) {
				bestIndex = index
				bestSuffixLength = len(candidate)
				bestOrder = resolver.Order
			}
		}
	}
	if bestIndex == -1 {
		return Resolver{}, fmt.Errorf("macOS has no resolver for DNS name %q", questionName)
	}
	return resolvers[bestIndex], nil
}

func domainMatches(name, suffix string) bool {
	return name == suffix || strings.HasSuffix(name, "."+suffix)
}

func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

func cloneResolvers(resolvers []Resolver) []Resolver {
	cloned := make([]Resolver, len(resolvers))
	for index, resolver := range resolvers {
		cloned[index] = resolver
		cloned[index].Domains = append([]string(nil), resolver.Domains...)
		cloned[index].Nameservers = append([]netip.Addr(nil), resolver.Nameservers...)
	}
	return cloned
}

// ScutilResolverSource parses the public macOS dynamic-store DNS report.
type ScutilResolverSource struct{}

func (ScutilResolverSource) Load(ctx context.Context) ([]Resolver, error) {
	output, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--dns").Output()
	if err != nil {
		return nil, err
	}
	return ParseScutilDNS(string(output))
}

// ParseScutilDNS parses resolver blocks while ignoring interface identifiers
// and other machine-specific metadata.
func ParseScutilDNS(output string) ([]Resolver, error) {
	type builder struct {
		resolver Resolver
		seen     bool
	}
	current := builder{}
	resolvers := make([]Resolver, 0, 8)
	flush := func() {
		if !current.seen || len(current.resolver.Nameservers) == 0 {
			current = builder{}
			return
		}
		if current.resolver.Port == 0 {
			current.resolver.Port = defaultDNSPort
		}
		current.resolver.Domains = uniqueDomains(current.resolver.Domains)
		resolvers = append(resolvers, current.resolver)
		current = builder{}
	}

	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "resolver #") {
			flush()
			current.seen = true
			continue
		}
		if !current.seen {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch {
		case key == "domain" || strings.HasPrefix(key, "search domain["):
			if domain := normalizeDomain(value); domain != "" {
				current.resolver.Domains = append(current.resolver.Domains, domain)
			}
		case strings.HasPrefix(key, "nameserver["):
			if address, parseError := netip.ParseAddr(value); parseError == nil && address.Is4() {
				current.resolver.Nameservers = append(current.resolver.Nameservers, address)
			}
		case key == "port":
			if port, parseError := strconv.ParseUint(value, 10, 16); parseError == nil && port > 0 {
				current.resolver.Port = uint16(port)
			}
		case key == "order":
			if order, parseError := strconv.Atoi(value); parseError == nil {
				current.resolver.Order = order
			}
		}
	}
	flush()
	if len(resolvers) == 0 {
		return nil, errors.New("scutil reported no usable IPv4 DNS resolvers")
	}
	sort.SliceStable(resolvers, func(left, right int) bool {
		return resolvers[left].Order < resolvers[right].Order
	})
	return resolvers, nil
}

func uniqueDomains(domains []string) []string {
	seen := make(map[string]struct{}, len(domains))
	result := make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = normalizeDomain(domain)
		if domain == "" {
			continue
		}
		if _, exists := seen[domain]; exists {
			continue
		}
		seen[domain] = struct{}{}
		result = append(result, domain)
	}
	return result
}
