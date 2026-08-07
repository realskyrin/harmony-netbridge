// harmony-netbridge is the macOS USB VPN bridge CLI and background supervisor.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/realskyrin/harmony-netbridge/internal/apperror"
	"github.com/realskyrin/harmony-netbridge/internal/control"
	"github.com/realskyrin/harmony-netbridge/internal/daemon"
	"github.com/realskyrin/harmony-netbridge/internal/hdc"
	"github.com/realskyrin/harmony-netbridge/internal/logging"
	proxybridge "github.com/realskyrin/harmony-netbridge/internal/proxy"
	"github.com/realskyrin/harmony-netbridge/internal/runtimepath"
	"github.com/realskyrin/harmony-netbridge/internal/state"
	"github.com/realskyrin/harmony-netbridge/internal/version"
)

const (
	startupTimeout  = 10 * time.Second
	shutdownTimeout = 8 * time.Second
	requestTimeout  = 2 * time.Second
)

type invocation struct {
	command       string
	hdcPath       string
	deviceID      string
	deviceLabel   string
	devicePort    int
	mtu           int
	mtuSet        bool
	proxyMode     bool
	proxyPort     int
	proxyPortSet  bool
	webPort       int
	webPortSet    bool
	mitmwebPath   string
	upstreamURL   string
	sslInsecure   bool
	captureFile   string
	noOpenBrowser bool
	help          bool
	version       bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	parsed, err := parseInvocation(arguments)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n\n", err)
		printUsage(stderr)
		return 2
	}
	if parsed.help {
		printUsage(stdout)
		return 0
	}
	if parsed.version {
		fmt.Fprintf(stdout, "harmony-netbridge %s (%s)\n", version.Version, version.Commit)
		return 0
	}

	paths, err := runtimepath.Default()
	if err != nil {
		return printError(stderr, err)
	}
	switch parsed.command {
	case "start":
		err = startCommand(parsed, paths, stdout)
	case "proxy":
		err = startCommand(parsed, paths, stdout)
	case "status":
		err = statusCommand(paths, stdout)
	case "stop":
		err = stopCommand(paths, stdout)
	case "__daemon":
		err = daemonCommand(parsed, paths)
	default:
		err = errors.New("a command is required")
	}
	if err != nil {
		return printError(stderr, err)
	}
	return 0
}

func startCommand(parsed invocation, paths runtimepath.Paths, output io.Writer) error {
	wantsProxy := parsed.command == "proxy" || parsed.proxyMode
	if response, err := callDaemon(paths, control.CommandStatus, 300*time.Millisecond); err == nil && response.OK {
		if response.State.Daemon == state.DaemonRunning {
			if response.State.Proxy.Enabled != wantsProxy {
				return apperror.New(apperror.CodeDaemonRunning,
					"HarmonyNetBridge is already running in a different mode; stop it before switching modes")
			}
			if parsed.mtuSet && response.State.MTU != 0 && response.State.MTU != parsed.mtu {
				return apperror.New(apperror.CodeDaemonRunning,
					fmt.Sprintf("HarmonyNetBridge is already running with MTU %d; stop it before changing MTU", response.State.MTU))
			}
			if wantsProxy {
				proxyChanged := (parsed.proxyPortSet && response.State.Proxy.ListenPort != parsed.proxyPort) ||
					(parsed.webPortSet && response.State.Proxy.WebPort != parsed.webPort) ||
					(parsed.noOpenBrowser && response.State.Proxy.OpenBrowser) ||
					response.State.Proxy.UpstreamURL != parsed.upstreamURL ||
					response.State.Proxy.SSLInsecure != parsed.sslInsecure
				if parsed.mitmwebPath != "" {
					resolved, discoverError := proxybridge.Discover(parsed.mitmwebPath)
					if discoverError != nil {
						return apperror.Wrap(apperror.CodeProxyUnavailable, discoverError.Error(), discoverError)
					}
					proxyChanged = proxyChanged || resolved != response.State.Proxy.Executable
				}
				if proxyChanged {
					return apperror.New(apperror.CodeDaemonRunning,
						"HarmonyNetBridge is already running with different proxy options; stop it before changing them")
				}
			}
			fmt.Fprintln(output, "HarmonyNetBridge is already running.")
			printStatus(output, response.State)
			return nil
		}
		if response.State.Daemon == state.DaemonFailed {
			return daemonFailure(response.State)
		}
		return apperror.New(apperror.CodeDaemonRunning, "a HarmonyNetBridge daemon is already starting or stopping")
	}

	hdcPath, err := hdc.Discover(parsed.hdcPath)
	if err != nil {
		return err
	}
	manager := hdc.NewManager(hdcPath)
	versionContext, cancelVersion := context.WithTimeout(context.Background(), requestTimeout)
	versionOutput, err := manager.Version(versionContext)
	cancelVersion()
	if err != nil || strings.TrimSpace(versionOutput) == "" {
		return apperror.Wrap(apperror.CodeHDCNotFound, "hdc was found but its version check failed", err)
	}
	listContext, cancelList := context.WithTimeout(context.Background(), requestTimeout)
	targets, err := manager.ListTargets(listContext)
	cancelList()
	if err != nil {
		return apperror.Wrap(apperror.CodeNoDevice, "hdc could not list Harmony devices", err)
	}
	target, err := hdc.SelectTarget(targets, parsed.deviceID)
	if err != nil {
		return err
	}

	if err := paths.Ensure(); err != nil {
		return err
	}
	if wantsProxy {
		parsed.mitmwebPath, err = proxybridge.Discover(parsed.mitmwebPath)
		if err != nil {
			return apperror.Wrap(apperror.CodeProxyUnavailable, err.Error(), err)
		}
		previous, _ := state.ReadFile(paths.StateFile)
		mayRecoverOwnedProxy := previous.Proxy.Enabled && previous.Proxy.ListenPort == parsed.proxyPort &&
			previous.Proxy.WebPort == parsed.webPort
		if !mayRecoverOwnedProxy {
			if err := proxybridge.CheckLoopbackPorts(parsed.proxyPort, parsed.webPort); err != nil {
				return apperror.Wrap(apperror.CodePortConflict, err.Error(), err)
			}
		}
		parsed.captureFile, err = proxybridge.NewCapturePath(paths.CaptureDir, time.Now())
		if err != nil {
			return apperror.Wrap(apperror.CodeProxyUnavailable, "a private capture filename could not be created", err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve harmony-netbridge executable: %w", err)
	}
	logFile, err := os.OpenFile(paths.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon bootstrap log: %w", err)
	}
	defer logFile.Close()
	if err := os.Chmod(paths.LogFile, 0o600); err != nil {
		return fmt.Errorf("protect daemon bootstrap log: %w", err)
	}
	childArguments := []string{
		"__daemon",
		"--hdc", hdcPath,
		"--device", target.ID,
		"--device-label", target.RedactedName(),
		"--device-port", strconv.Itoa(parsed.devicePort),
		"--mtu", strconv.Itoa(parsed.mtu),
	}
	if wantsProxy {
		childArguments = append(childArguments,
			"--proxy-mode",
			"--proxy-port", strconv.Itoa(parsed.proxyPort),
			"--web-port", strconv.Itoa(parsed.webPort),
			"--mitmweb", parsed.mitmwebPath,
			"--capture-file", parsed.captureFile,
		)
		if parsed.upstreamURL != "" {
			childArguments = append(childArguments, "--upstream", parsed.upstreamURL)
		}
		if parsed.sslInsecure {
			childArguments = append(childArguments, "--ssl-insecure")
		}
		if parsed.noOpenBrowser {
			childArguments = append(childArguments, "--no-open-browser")
		}
	}
	command := exec.Command(executable, childArguments...)
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	childStartedAt := time.Now().UTC()
	if err := command.Start(); err != nil {
		return fmt.Errorf("start HarmonyNetBridge daemon: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		_ = command.Process.Kill()
		return fmt.Errorf("release HarmonyNetBridge daemon process: %w", err)
	}

	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		response, callErr := callDaemon(paths, control.CommandStatus, 300*time.Millisecond)
		if callErr == nil && response.OK {
			if response.State.Daemon == state.DaemonRunning {
				fmt.Fprintln(output, "HarmonyNetBridge started.")
				printStatus(output, response.State)
				return nil
			}
			if response.State.Daemon == state.DaemonFailed {
				return daemonFailure(response.State)
			}
		}
		if daemonError := recentDaemonFailure(paths, childStartedAt); daemonError != nil {
			return daemonError
		}
		time.Sleep(75 * time.Millisecond)
	}
	if daemonError := recentDaemonFailure(paths, childStartedAt); daemonError != nil {
		return daemonError
	}
	return apperror.New(
		apperror.CodeDaemonUnavailable,
		"the daemon did not become ready within 10 seconds. Check ~/Library/Logs/HarmonyNetBridge/harmony-netbridge.log",
	)
}

func statusCommand(paths runtimepath.Paths, output io.Writer) error {
	response, err := callDaemon(paths, control.CommandStatus, requestTimeout)
	if err != nil {
		if control.IsUnavailable(err) {
			printStatus(output, state.Stopped(time.Now()))
			return nil
		}
		return err
	}
	if !response.OK {
		return apperror.New(response.ErrorCode, response.Error)
	}
	printStatus(output, response.State)
	return nil
}

func stopCommand(paths runtimepath.Paths, output io.Writer) error {
	stopRequestedAt := time.Now().UTC()
	response, err := callDaemon(paths, control.CommandStop, requestTimeout)
	if err != nil {
		if control.IsUnavailable(err) {
			fmt.Fprintln(output, "HarmonyNetBridge is already stopped.")
			return nil
		}
		return err
	}
	if !response.OK {
		return apperror.New(response.ErrorCode, response.Error)
	}

	deadline := time.Now().Add(shutdownTimeout)
	for time.Now().Before(deadline) {
		_, callErr := callDaemon(paths, control.CommandStatus, 250*time.Millisecond)
		if callErr != nil && control.IsUnavailable(callErr) {
			if daemonError := recentDaemonFailure(paths, stopRequestedAt); daemonError != nil {
				return daemonError
			}
			fmt.Fprintln(output, "HarmonyNetBridge stopped.")
			return nil
		}
		time.Sleep(75 * time.Millisecond)
	}
	return apperror.New(apperror.CodeDaemonUnavailable, "the daemon did not stop within 8 seconds")
}

func recentDaemonFailure(paths runtimepath.Paths, notBefore time.Time) error {
	snapshot, err := state.ReadFile(paths.StateFile)
	if err != nil || snapshot.Daemon != state.DaemonFailed || snapshot.UpdatedAt.Before(notBefore) {
		return nil
	}
	return daemonFailure(snapshot)
}

func daemonFailure(snapshot state.Snapshot) error {
	code := apperror.Code(snapshot.LastErrorCode)
	if code == "" {
		code = apperror.CodeDaemonUnavailable
	}
	message := snapshot.LastError
	if message == "" {
		message = snapshot.Message
	}
	if message == "" {
		message = "the HarmonyNetBridge daemon stopped unexpectedly"
	}
	return apperror.New(code, message)
}

func daemonCommand(parsed invocation, paths runtimepath.Paths) error {
	if parsed.hdcPath == "" || parsed.deviceID == "" || parsed.deviceLabel == "" {
		return errors.New("internal daemon arguments are incomplete")
	}
	if parsed.proxyMode && (parsed.mitmwebPath == "" || parsed.captureFile == "") {
		return errors.New("internal proxy daemon arguments are incomplete")
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	logger, writer, err := logging.New(paths.LogFile, logging.DefaultMaxBytes)
	if err != nil {
		return err
	}
	defer writer.Close()
	logger.Info("daemon starting", "version", version.Version, "device", parsed.deviceLabel)

	manager := hdc.NewManager(parsed.hdcPath)
	var proxyFactory daemon.ProxyFactory
	if parsed.proxyMode {
		proxyConfig := proxybridge.Config{
			Executable:  parsed.mitmwebPath,
			ListenPort:  parsed.proxyPort,
			WebPort:     parsed.webPort,
			OpenBrowser: !parsed.noOpenBrowser,
			CaptureFile: parsed.captureFile,
			ConfDir:     paths.ProxyConfDir,
			LogFile:     paths.ProxyLogFile,
			UpstreamURL: parsed.upstreamURL,
			SSLInsecure: parsed.sslInsecure,
		}
		proxyFactory = func(_ state.ProxySnapshot) (daemon.ProxySession, error) {
			return proxybridge.Start(proxyConfig)
		}
	}
	server, err := daemon.New(daemon.Config{
		Paths:         paths,
		DeviceID:      parsed.deviceID,
		DeviceLabel:   parsed.deviceLabel,
		DevicePort:    parsed.devicePort,
		MTU:           parsed.mtu,
		Forwarder:     manager,
		Logger:        logger,
		ProxyFactory:  proxyFactory,
		ProxyRecovery: proxybridge.RecoverManaged,
	})
	if err != nil {
		logger.Error("daemon configuration failed", "error", err)
		return err
	}
	contextWithSignal, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := server.Run(contextWithSignal); err != nil {
		logger.Error("daemon stopped with error", "error", err)
		return err
	}
	return nil
}

func callDaemon(paths runtimepath.Paths, command control.Command, timeout time.Duration) (control.Response, error) {
	requestContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return control.Call(requestContext, paths.ControlSocket, control.Request{Command: command})
}

func printStatus(output io.Writer, snapshot state.Snapshot) {
	device := snapshot.Device
	if device == "" {
		device = "None"
	} else {
		device = "Harmony device (" + device + ")"
	}
	message := snapshot.Message
	if message == "" {
		message = "No status message"
	}
	fmt.Fprintf(output, "Daemon:    %s\n", snapshot.Daemon)
	fmt.Fprintf(output, "Device:    %s\n", device)
	fmt.Fprintf(output, "Transport: %s\n", snapshot.Transport)
	fmt.Fprintf(output, "VPN:       %s\n", snapshot.VPN)
	if snapshot.MTU > 0 {
		fmt.Fprintf(output, "MTU:       %d\n", snapshot.MTU)
	}
	if snapshot.Proxy.Enabled {
		fmt.Fprintf(output, "Proxy:     %s (HTTP/HTTPS TCP)\n", snapshot.Proxy.Status)
		if snapshot.Proxy.WebPort > 0 {
			if snapshot.Proxy.OpenBrowser {
				fmt.Fprintf(output, "Proxy UI:  opened by mitmweb on 127.0.0.1:%d\n", snapshot.Proxy.WebPort)
			} else {
				fmt.Fprintf(output, "Proxy UI:  loopback 127.0.0.1:%d (browser launch disabled)\n", snapshot.Proxy.WebPort)
			}
		}
		if snapshot.Proxy.CaptureFile != "" {
			fmt.Fprintf(output, "Capture:   %s\n", snapshot.Proxy.CaptureFile)
		}
		if snapshot.Proxy.CACertFile != "" {
			fmt.Fprintf(output, "HTTPS CA:  App downloads from %s (system confirmation required)\n", snapshot.Proxy.CACertFile)
		}
		if snapshot.Proxy.SSLInsecure {
			fmt.Fprintln(output, "TLS verify: DISABLED for mitmweb upstream connections")
		}
	}
	if snapshot.Daemon == state.DaemonRunning && !snapshot.StartedAt.IsZero() {
		fmt.Fprintf(output, "Uptime:    %s\n", formatDuration(time.Since(snapshot.StartedAt)))
	}
	if snapshot.ControlHeartbeatAt.IsZero() && snapshot.DataHeartbeatAt.IsZero() {
		if snapshot.Transport == state.TransportDataConnected {
			fmt.Fprintln(output, "Heartbeat: waiting")
		}
	} else {
		fmt.Fprintf(output, "Heartbeat: control %d ms / data %d ms\n", snapshot.ControlRTTMillis, snapshot.DataRTTMillis)
	}
	if snapshot.Relay.PacketsFromDevice > 0 || snapshot.Relay.PacketsToDevice > 0 {
		fmt.Fprintf(output, "Traffic:   device→Mac %s (%d packets) / Mac→device %s (%d packets)\n",
			formatBytes(snapshot.Relay.BytesFromDevice), snapshot.Relay.PacketsFromDevice,
			formatBytes(snapshot.Relay.BytesToDevice), snapshot.Relay.PacketsToDevice)
		fmt.Fprintf(output, "Flows:     TCP %d / UDP %d / DNS %d\n",
			snapshot.Relay.TCPFlows, snapshot.Relay.UDPFlows, snapshot.Relay.DNSQueries)
		if snapshot.Proxy.Enabled {
			fmt.Fprintf(output, "Intercept: proxied TCP %d / QUIC fallbacks %d\n",
				snapshot.Relay.ProxyTCPFlows, snapshot.Relay.BlockedQUICFlows)
		}
	}
	if snapshot.Reconnects > 0 {
		fmt.Fprintf(output, "Reconnects: %d\n", snapshot.Reconnects)
	}
	fmt.Fprintf(output, "Message:   %s\n", message)
}

func formatBytes(value uint64) string {
	const unit = uint64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	amount := float64(value)
	unitIndex := -1
	for amount >= float64(unit) && unitIndex+1 < len(units) {
		amount /= float64(unit)
		unitIndex++
	}
	return fmt.Sprintf("%.1f %s", amount, units[unitIndex])
}

func formatDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	return duration.Round(time.Second).String()
}

func printError(output io.Writer, err error) int {
	code, message := apperror.Details(err)
	fmt.Fprintf(output, "[%s] %s\n", code, message)
	switch code {
	case apperror.CodeHDCNotFound, apperror.CodeNoDevice, apperror.CodeDeviceOffline,
		apperror.CodeMultipleDevices, apperror.CodeDeviceNotFound:
		return 3
	case apperror.CodeDaemonRunning, apperror.CodeDaemonUnavailable,
		apperror.CodePortConflict, apperror.CodeRPortFailed, apperror.CodeProxyUnavailable:
		return 4
	case apperror.CodeHandshakeTimeout, apperror.CodeVersionUnsupported,
		apperror.CodeAppDisconnected:
		return 5
	default:
		return 1
	}
}

func parseInvocation(arguments []string) (invocation, error) {
	parsed := invocation{
		devicePort: daemon.DefaultDevicePort,
		mtu:        daemon.DefaultMTU,
		proxyPort:  proxybridge.DefaultListenPort,
		webPort:    proxybridge.DefaultWebPort,
	}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch argument {
		case "start", "proxy", "status", "stop", "__daemon":
			if parsed.command != "" {
				return invocation{}, fmt.Errorf("multiple commands were provided: %s and %s", parsed.command, argument)
			}
			parsed.command = argument
		case "--help", "-h":
			parsed.help = true
		case "--version":
			parsed.version = true
		case "--proxy-mode":
			parsed.proxyMode = true
		case "--ssl-insecure":
			parsed.sslInsecure = true
		case "--no-open-browser":
			parsed.noOpenBrowser = true
		case "--hdc", "--device", "--device-label", "--device-port", "--mtu",
			"--proxy-port", "--web-port", "--mitmweb", "--upstream", "--capture-file":
			if index+1 >= len(arguments) {
				return invocation{}, fmt.Errorf("%s requires a value", argument)
			}
			index++
			if err := assignOption(&parsed, argument, arguments[index]); err != nil {
				return invocation{}, err
			}
		default:
			name, value, found := strings.Cut(argument, "=")
			if found && (name == "--hdc" || name == "--device" || name == "--device-label" ||
				name == "--device-port" || name == "--mtu" || name == "--proxy-port" ||
				name == "--web-port" || name == "--mitmweb" || name == "--upstream" || name == "--capture-file") {
				if err := assignOption(&parsed, name, value); err != nil {
					return invocation{}, err
				}
				continue
			}
			return invocation{}, fmt.Errorf("unknown argument %q", argument)
		}
	}
	if parsed.command == "" && !parsed.help && !parsed.version {
		return invocation{}, errors.New("a command is required")
	}
	if parsed.command != "__daemon" && parsed.deviceLabel != "" {
		return invocation{}, errors.New("--device-label is an internal option")
	}
	if parsed.command != "__daemon" && (parsed.proxyMode || parsed.captureFile != "") {
		return invocation{}, errors.New("--proxy-mode and --capture-file are internal options")
	}
	if parsed.command == "proxy" {
		parsed.proxyMode = true
	}
	if parsed.mtuSet && parsed.command != "start" && parsed.command != "proxy" && parsed.command != "__daemon" {
		return invocation{}, errors.New("--mtu is only valid with start or proxy")
	}
	proxyOptionSet := parsed.proxyPortSet || parsed.webPortSet || parsed.mitmwebPath != "" ||
		parsed.upstreamURL != "" || parsed.sslInsecure || parsed.noOpenBrowser
	if proxyOptionSet && parsed.command != "proxy" && !(parsed.command == "__daemon" && parsed.proxyMode) {
		return invocation{}, errors.New("proxy options are only valid with proxy")
	}
	if parsed.proxyPort == parsed.webPort && (parsed.command == "proxy" || parsed.proxyMode) {
		return invocation{}, errors.New("--proxy-port and --web-port must differ")
	}
	return parsed, nil
}

func assignOption(parsed *invocation, name, value string) error {
	if value == "" {
		return fmt.Errorf("%s cannot be empty", name)
	}
	switch name {
	case "--hdc":
		parsed.hdcPath = value
	case "--device":
		parsed.deviceID = value
	case "--device-label":
		parsed.deviceLabel = value
	case "--device-port":
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65_535 {
			return errors.New("--device-port must be an integer in 1...65535")
		}
		parsed.devicePort = port
	case "--mtu":
		mtu, err := strconv.Atoi(value)
		if err != nil || mtu < daemon.MinimumMTU || mtu > daemon.MaximumMTU {
			return fmt.Errorf("--mtu must be an integer in %d...%d", daemon.MinimumMTU, daemon.MaximumMTU)
		}
		parsed.mtu = mtu
		parsed.mtuSet = true
	case "--proxy-port":
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65_535 {
			return errors.New("--proxy-port must be an integer in 1...65535")
		}
		parsed.proxyPort = port
		parsed.proxyPortSet = true
	case "--web-port":
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65_535 {
			return errors.New("--web-port must be an integer in 1...65535")
		}
		parsed.webPort = port
		parsed.webPortSet = true
	case "--mitmweb":
		parsed.mitmwebPath = value
	case "--upstream":
		if err := proxybridge.ValidateUpstreamURL(value); err != nil {
			return fmt.Errorf("--upstream %w", err)
		}
		parsed.upstreamURL = value
	case "--capture-file":
		parsed.captureFile = value
	}
	return nil
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, `HarmonyNetBridge Phase 4

Usage:
  harmony-netbridge [--hdc <path>] [--device <target>] [--mtu <bytes>] start
  harmony-netbridge [--hdc <path>] [--device <target>] [--mtu <bytes>] proxy
  harmony-netbridge status
  harmony-netbridge stop

Commands:
  start    Start the per-user bridge daemon and USB packet relay
  proxy    Start the VPN relay plus a managed mitmweb capture session
  status   Read live daemon, transport, and VPN state
  stop     Stop the daemon and remove only its owned hdc mapping

Options:
  --hdc <path>       Explicit hdc executable (also HARMONY_NETBRIDGE_HDC)
  --device <target>  Select one target when multiple devices are Connected
  --mtu <bytes>      VPN MTU in 576...1500 (default 1400)
  --mitmweb <path>   Explicit mitmweb executable (also HARMONY_NETBRIDGE_MITMWEB)
  --upstream <URL>   Route mitmweb through an HTTP(S) upstream proxy
  --ssl-insecure     Disable mitmweb upstream TLS certificate verification
  --proxy-port <n>   Loopback proxy port (default 8080)
  --web-port <n>     Loopback mitmweb UI port (default 8081)
  --no-open-browser  Do not let mitmweb open its authenticated Web UI
  --version          Print the CLI version
  -h, --help         Show this help`)
}
