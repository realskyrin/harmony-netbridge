package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/realskyrin/harmony-netbridge/internal/apperror"
	"github.com/realskyrin/harmony-netbridge/internal/daemon"
	"github.com/realskyrin/harmony-netbridge/internal/runtimepath"
	"github.com/realskyrin/harmony-netbridge/internal/state"
)

func TestParseInvocationAcceptsOptionsBeforeOrAfterCommand(t *testing.T) {
	tests := [][]string{
		{"--hdc", "/tmp/hdc", "--device", "one", "start"},
		{"start", "--hdc=/tmp/hdc", "--device=one"},
	}
	for _, arguments := range tests {
		parsed, err := parseInvocation(arguments)
		if err != nil {
			t.Fatalf("parseInvocation(%v) error = %v", arguments, err)
		}
		if parsed.command != "start" || parsed.hdcPath != "/tmp/hdc" || parsed.deviceID != "one" {
			t.Fatalf("parseInvocation(%v) = %#v", arguments, parsed)
		}
		if parsed.devicePort != daemon.DefaultDevicePort {
			t.Fatalf("device port = %d", parsed.devicePort)
		}
		if parsed.mtu != daemon.DefaultMTU {
			t.Fatalf("MTU = %d", parsed.mtu)
		}
	}
}

func TestParseInvocationAcceptsConfiguredMTU(t *testing.T) {
	parsed, err := parseInvocation([]string{"start", "--mtu", "1280"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.mtu != 1280 {
		t.Fatalf("MTU = %d, want 1280", parsed.mtu)
	}
}

func TestParseInvocationAcceptsProxyMode(t *testing.T) {
	parsed, err := parseInvocation([]string{
		"proxy", "--mtu", "1280", "--proxy-port", "9080", "--web-port=9081",
		"--mitmweb", "/opt/homebrew/bin/mitmweb", "--no-open-browser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.proxyMode || parsed.proxyPort != 9080 || parsed.webPort != 9081 ||
		parsed.mitmwebPath != "/opt/homebrew/bin/mitmweb" || !parsed.noOpenBrowser {
		t.Fatalf("proxy invocation = %#v", parsed)
	}
}

func TestRecentDaemonFailureRejectsStaleStateAndReturnsCurrentError(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("/tmp", "hnb-main-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	paths, err := runtimepath.FromRoots(filepath.Join(root, "runtime"), filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := state.NewStarting(now, "device-redacted", daemon.DefaultDevicePort)
	snapshot.Daemon = state.DaemonFailed
	snapshot.LastErrorCode = string(apperror.CodeRPortFailed)
	snapshot.LastError = "mapping failed"
	snapshot.UpdatedAt = now
	if err := state.WriteFile(paths.StateFile, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := recentDaemonFailure(paths, now.Add(time.Second)); err != nil {
		t.Fatalf("stale state returned error: %v", err)
	}
	err = recentDaemonFailure(paths, now.Add(-time.Second))
	var appError *apperror.Error
	if !errors.As(err, &appError) || appError.Code != apperror.CodeRPortFailed || appError.Message != "mapping failed" {
		t.Fatalf("recentDaemonFailure() = %v", err)
	}
	if err := os.Remove(paths.StateFile); err != nil {
		t.Fatal(err)
	}
	if err := recentDaemonFailure(paths, time.Time{}); err != nil {
		t.Fatalf("missing state returned error: %v", err)
	}
}

func TestParseInvocationRejectsUnknownAndMultipleCommands(t *testing.T) {
	for _, arguments := range [][]string{
		{"devices"}, {"start", "stop"}, {"start", "--device-port", "0"},
		{"start", "--mtu", "575"}, {"start", "--mtu", "1501"},
		{"status", "--mtu", "1280"}, {"start", "--proxy-port", "8080"},
		{"proxy", "--proxy-port", "8081", "--web-port", "8081"},
		{"proxy", "--capture-file", "/tmp/not-public"},
	} {
		if _, err := parseInvocation(arguments); err == nil {
			t.Fatalf("parseInvocation(%v) error = nil", arguments)
		}
	}
}

func TestPrintStatusShowsSafeProxyMetadata(t *testing.T) {
	var output bytes.Buffer
	printStatus(&output, state.Snapshot{
		Daemon:    state.DaemonRunning,
		Transport: state.TransportDataConnected,
		VPN:       state.VPNActive,
		Proxy: state.ProxySnapshot{
			Enabled:     true,
			Status:      state.ProxyActive,
			WebPort:     8081,
			CaptureFile: "/private/capture.mitm",
			CACertFile:  "/private/mitmproxy-ca-cert.cer",
			PID:         1234,
			Executable:  "/private/mitmweb",
		},
		Relay: state.RelayStats{PacketsFromDevice: 1, ProxyTCPFlows: 2, BlockedQUICFlows: 3},
	})
	text := output.String()
	if !strings.Contains(text, "Proxy:     ACTIVE") || !strings.Contains(text, "Intercept: proxied TCP 2 / QUIC fallbacks 3") {
		t.Fatalf("proxy status = %s", text)
	}
	if strings.Contains(text, "1234") || strings.Contains(text, "/private/mitmweb") {
		t.Fatalf("proxy process metadata leaked: %s", text)
	}
}

func TestPrintStatusDoesNotExposeRawDeviceID(t *testing.T) {
	var output bytes.Buffer
	printStatus(&output, state.Snapshot{
		Daemon:    state.DaemonRunning,
		Transport: state.TransportPortReady,
		VPN:       state.VPNStopped,
		Device:    "device-deadbeef",
		MTU:       1280,
		Relay: state.RelayStats{
			PacketsFromDevice: 2,
			BytesFromDevice:   2048,
			PacketsToDevice:   1,
			BytesToDevice:     1024,
		},
		Message: "Waiting for HarmonyNetBridge App",
	})
	text := output.String()
	if !strings.Contains(text, "device-deadbeef") || strings.Contains(text, "secret-device-id") {
		t.Fatalf("unexpected status output: %s", text)
	}
	if !strings.Contains(text, "MTU:       1280") || !strings.Contains(text, "device→Mac 2.0 KiB") {
		t.Fatalf("status is missing Phase 3 metrics: %s", text)
	}
}

func TestStatusWithoutDaemonIsStopped(t *testing.T) {
	// run() uses the real per-user path, so this test targets the stable formatter
	// and parser without interfering with a developer's live daemon.
	var output bytes.Buffer
	printStatus(&output, state.Stopped(time.Time{}))
	if !strings.Contains(output.String(), "Daemon:    STOPPED") {
		t.Fatalf("status output = %q", output.String())
	}
}
