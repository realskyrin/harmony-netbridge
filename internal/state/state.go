// Package state models daemon, transport, and VPN state independently.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type DaemonStatus string
type TransportStatus string
type VPNStatus string

const (
	DaemonStopped  DaemonStatus = "STOPPED"
	DaemonStarting DaemonStatus = "STARTING"
	DaemonRunning  DaemonStatus = "RUNNING"
	DaemonStopping DaemonStatus = "STOPPING"
	DaemonFailed   DaemonStatus = "FAILED"

	TransportNoDevice         TransportStatus = "NO_DEVICE"
	TransportDeviceOffline    TransportStatus = "DEVICE_OFFLINE"
	TransportPortReady        TransportStatus = "PORT_READY"
	TransportControlConnected TransportStatus = "CONTROL_CONNECTED"
	TransportDataConnected    TransportStatus = "DATA_CONNECTED"

	VPNUnavailable  VPNStatus = "UNAVAILABLE"
	VPNAuthRequired VPNStatus = "AUTH_REQUIRED"
	VPNStarting     VPNStatus = "STARTING"
	VPNReconnecting VPNStatus = "RECONNECTING"
	VPNActive       VPNStatus = "ACTIVE"
	VPNStopped      VPNStatus = "STOPPED"
	VPNFailed       VPNStatus = "FAILED"
)

// RelayStats contains aggregate, payload-free data-plane counters. It never
// contains addresses, ports, packet contents, or session identifiers.
type RelayStats struct {
	PacketsFromDevice uint64 `json:"packetsFromDevice"`
	BytesFromDevice   uint64 `json:"bytesFromDevice"`
	PacketsToDevice   uint64 `json:"packetsToDevice"`
	BytesToDevice     uint64 `json:"bytesToDevice"`
	TCPFlows          uint64 `json:"tcpFlows"`
	UDPFlows          uint64 `json:"udpFlows"`
	DNSQueries        uint64 `json:"dnsQueries"`
}

// Snapshot is returned by status and persisted for diagnostics. Device is always redacted.
type Snapshot struct {
	SchemaVersion      int             `json:"schemaVersion"`
	Daemon             DaemonStatus    `json:"daemon"`
	Transport          TransportStatus `json:"transport"`
	VPN                VPNStatus       `json:"vpn"`
	Device             string          `json:"device,omitempty"`
	DevicePort         int             `json:"devicePort,omitempty"`
	HostPort           int             `json:"hostPort,omitempty"`
	MTU                int             `json:"mtu,omitempty"`
	Reconnects         uint64          `json:"reconnects,omitempty"`
	ControlHeartbeatAt time.Time       `json:"controlHeartbeatAt,omitempty"`
	ControlRTTMillis   int64           `json:"controlRttMillis,omitempty"`
	DataHeartbeatAt    time.Time       `json:"dataHeartbeatAt,omitempty"`
	DataRTTMillis      int64           `json:"dataRttMillis,omitempty"`
	Relay              RelayStats      `json:"relay"`
	Message            string          `json:"message,omitempty"`
	LastErrorCode      string          `json:"lastErrorCode,omitempty"`
	LastError          string          `json:"lastError,omitempty"`
	StartedAt          time.Time       `json:"startedAt,omitempty"`
	UpdatedAt          time.Time       `json:"updatedAt"`
}

// NewStarting creates the first daemon state.
func NewStarting(now time.Time, device string, devicePort int) Snapshot {
	return Snapshot{
		SchemaVersion: 2,
		Daemon:        DaemonStarting,
		Transport:     TransportNoDevice,
		VPN:           VPNStopped,
		Device:        device,
		DevicePort:    devicePort,
		Message:       "Starting HarmonyNetBridge",
		StartedAt:     now.UTC(),
		UpdatedAt:     now.UTC(),
	}
}

// Stopped creates a status response when no daemon is reachable.
func Stopped(now time.Time) Snapshot {
	return Snapshot{
		SchemaVersion: 2,
		Daemon:        DaemonStopped,
		Transport:     TransportNoDevice,
		VPN:           VPNStopped,
		Message:       "HarmonyNetBridge is stopped",
		UpdatedAt:     now.UTC(),
	}
}

// Store serializes updates from socket and control goroutines.
type Store struct {
	mu       sync.RWMutex
	snapshot Snapshot
}

func NewStore(initial Snapshot) *Store { return &Store{snapshot: initial} }

// Get returns a copy of the current snapshot.
func (s *Store) Get() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

// Update mutates the snapshot under a lock and refreshes UpdatedAt.
func (s *Store) Update(now time.Time, mutate func(*Snapshot)) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	mutate(&s.snapshot)
	s.snapshot.UpdatedAt = now.UTC()
	return s.snapshot
}

// WriteFile atomically persists a snapshot with user-only permissions.
func WriteFile(path string, snapshot Snapshot) error {
	payload, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state snapshot: %w", err)
	}
	temporary := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(temporary, append(payload, '\n'), 0o600); err != nil {
		return fmt.Errorf("write temporary state: %w", err)
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("protect temporary state: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace state snapshot: %w", err)
	}
	return nil
}

// ReadFile loads a persisted snapshot for diagnostics.
func ReadFile(path string) (Snapshot, error) {
	payload, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode state snapshot: %w", err)
	}
	return snapshot, nil
}
