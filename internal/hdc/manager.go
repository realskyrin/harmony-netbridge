// Package hdc discovers the DevEco hdc binary, selects one device, and manages
// the exact reverse-port mapping owned by HarmonyNetBridge.
package hdc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/realskyrin/harmony-netbridge/internal/apperror"
)

const (
	EnvironmentPath = "HARMONY_NETBRIDGE_HDC"
	defaultHDCPath  = "/Applications/DevEco-Studio.app/Contents/sdk/default/openharmony/toolchains/hdc"
)

// Runner executes hdc and exists as an interface so parsers and mapping
// ownership can be tested without mutating a real device.
type Runner interface {
	Run(ctx context.Context, executable string, args ...string) (string, error)
}

// ExecRunner executes the local hdc process.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, executable, args...).CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// Manager owns commands for one hdc executable.
type Manager struct {
	Path   string
	Runner Runner
}

// Target is one row returned by hdc list targets -v.
type Target struct {
	ID         string
	Connection string
	Status     string
	Endpoint   string
}

// InstalledApplication is one user-selectable application reported by the
// device Bundle Manager. Label may be empty on older device versions.
type InstalledApplication struct {
	BundleName string `json:"bundleName"`
	Label      string `json:"label"`
}

// Usable reports whether hdc considers this target ready for commands. Current
// hdc 3.x prints Connected; Online is accepted for compatibility with older or
// vendor-specific verbose output.
func (t Target) Usable() bool {
	return strings.EqualFold(t.Status, "Connected") || strings.EqualFold(t.Status, "Online")
}

// RedactedName returns a stable per-ID label without exposing the target ID.
func (t Target) RedactedName() string { return RedactTarget(t.ID) }

// Mapping identifies the exact reverse forwarding rule created by this process.
type Mapping struct {
	DevicePort int
	HostPort   int
}

// TaskString is the hdc identifier used for both creation and removal.
func (m Mapping) TaskString() string {
	return fmt.Sprintf("tcp:%d tcp:%d", m.DevicePort, m.HostPort)
}

// Discover finds hdc using the approved precedence order.
func Discover(explicit string) (string, error) {
	if explicit != "" {
		return validateExecutable(explicit)
	}
	if configured := os.Getenv(EnvironmentPath); configured != "" {
		return validateExecutable(configured)
	}
	if candidate, err := exec.LookPath("hdc"); err == nil {
		return filepath.Clean(candidate), nil
	}
	if candidate, err := validateExecutable(defaultHDCPath); err == nil {
		return candidate, nil
	}
	return "", apperror.New(
		apperror.CodeHDCNotFound,
		"hdc was not found. Install DevEco Studio, add hdc to PATH, or pass --hdc.",
	)
}

func validateExecutable(candidate string) (string, error) {
	cleaned, err := filepath.Abs(candidate)
	if err != nil {
		return "", apperror.Wrap(apperror.CodeHDCNotFound, "the configured hdc path is invalid", err)
	}
	info, err := os.Stat(cleaned)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", apperror.New(
			apperror.CodeHDCNotFound,
			"the configured hdc path does not point to an executable file",
		)
	}
	return cleaned, nil
}

// NewManager creates a manager backed by the real process runner.
func NewManager(path string) *Manager {
	return &Manager{Path: path, Runner: ExecRunner{}}
}

// Version returns hdc's version string.
func (m *Manager) Version(ctx context.Context) (string, error) {
	output, err := m.runner().Run(ctx, m.Path, "-v")
	if err != nil {
		return "", fmt.Errorf("query hdc version: %w", err)
	}
	if isFailureOutput(output) {
		return "", fmt.Errorf("query hdc version: hdc reported a failure")
	}
	return strings.TrimSpace(output), nil
}

// ListTargets returns all verbose target rows.
func (m *Manager) ListTargets(ctx context.Context) ([]Target, error) {
	output, err := m.runner().Run(ctx, m.Path, "list", "targets", "-v")
	if err != nil {
		return nil, fmt.Errorf("list hdc targets: %w", err)
	}
	if isFailureOutput(output) {
		return nil, fmt.Errorf("list hdc targets: hdc reported a failure")
	}
	return ParseTargets(output), nil
}

// ListInstalledApplications returns the applications installed for the active
// device user. Newer bm versions can return localized labels as JSON; older
// versions fall back to the bundle-name list.
func (m *Manager) ListInstalledApplications(ctx context.Context, targetID string) ([]InstalledApplication, error) {
	labelOutput, labelErr := m.runner().Run(ctx, m.Path, "-t", targetID, "shell", "bm", "dump", "-a", "-l")
	if labelErr == nil && !isFailureOutput(labelOutput) {
		if applications := ParseInstalledApplicationLabels(labelOutput); len(applications) > 0 {
			return applications, nil
		}
	}

	output, err := m.runner().Run(ctx, m.Path, "-t", targetID, "shell", "bm", "dump", "-a")
	if err != nil {
		return nil, fmt.Errorf("list installed applications: %w", commandFailure(err, output, targetID))
	}
	if isFailureOutput(output) {
		return nil, fmt.Errorf("list installed applications: hdc reported a failure")
	}
	applications := ParseInstalledApplicationNames(output)
	if len(applications) == 0 {
		return nil, fmt.Errorf("list installed applications: bm returned no valid applications")
	}
	return applications, nil
}

// ParseInstalledApplicationLabels parses bm dump -a -l JSON output.
func ParseInstalledApplicationLabels(output string) []InstalledApplication {
	trimmed := strings.TrimSpace(output)
	if len(trimmed) == 0 || len(trimmed) > 1024*1024 {
		return nil
	}
	var applications []InstalledApplication
	if err := json.Unmarshal([]byte(trimmed), &applications); err != nil {
		return nil
	}
	return normalizeInstalledApplications(applications)
}

// ParseInstalledApplicationNames parses bm dump -a output, including its
// "ID: <user>:" header, and ignores every line that is not a valid bundle name.
func ParseInstalledApplicationNames(output string) []InstalledApplication {
	if len(output) > 1024*1024 {
		return nil
	}
	applications := make([]InstalledApplication, 0)
	for _, line := range strings.Split(output, "\n") {
		applications = append(applications, InstalledApplication{BundleName: strings.TrimSpace(line)})
	}
	return normalizeInstalledApplications(applications)
}

func normalizeInstalledApplications(applications []InstalledApplication) []InstalledApplication {
	normalized := make([]InstalledApplication, 0, len(applications))
	seen := make(map[string]struct{}, len(applications))
	for _, application := range applications {
		bundleName := strings.TrimSpace(application.BundleName)
		if !isValidBundleName(bundleName) {
			continue
		}
		if _, exists := seen[bundleName]; exists {
			continue
		}
		label := strings.TrimSpace(application.Label)
		if len(label) > 256 || strings.ContainsAny(label, "\r\n\x00") {
			label = ""
		}
		seen[bundleName] = struct{}{}
		normalized = append(normalized, InstalledApplication{BundleName: bundleName, Label: label})
		if len(normalized) == 4096 {
			break
		}
	}
	sort.Slice(normalized, func(left, right int) bool {
		leftLabel := normalized[left].Label
		rightLabel := normalized[right].Label
		if leftLabel == "" {
			leftLabel = normalized[left].BundleName
		}
		if rightLabel == "" {
			rightLabel = normalized[right].BundleName
		}
		if strings.EqualFold(leftLabel, rightLabel) {
			return normalized[left].BundleName < normalized[right].BundleName
		}
		return strings.ToLower(leftLabel) < strings.ToLower(rightLabel)
	})
	return normalized
}

func isValidBundleName(bundleName string) bool {
	if len(bundleName) < 7 || len(bundleName) > 128 {
		return false
	}
	segments := strings.Split(bundleName, ".")
	if len(segments) < 3 {
		return false
	}
	for segmentIndex, segment := range segments {
		if segment == "" {
			return false
		}
		for characterIndex, character := range []byte(segment) {
			isLetter := (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z')
			isDigit := character >= '0' && character <= '9'
			if characterIndex == 0 && (!isLetter && (segmentIndex == 0 || !isDigit)) {
				return false
			}
			if !isLetter && !isDigit && character != '_' {
				return false
			}
			if characterIndex == len(segment)-1 && !isLetter && !isDigit {
				return false
			}
		}
	}
	return true
}

// ParseTargets parses both verbose hdc rows and the one-column fallback format.
func ParseTargets(output string) []Target {
	var targets []Target
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "[Empty]") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 1 {
			// Non-verbose hdc output only includes connected targets.
			targets = append(targets, Target{ID: fields[0], Status: "Connected"})
			continue
		}
		target := Target{ID: fields[0]}
		if len(fields) > 1 {
			target.Connection = fields[1]
		}
		if len(fields) > 2 {
			target.Status = fields[2]
		}
		if len(fields) > 3 {
			target.Endpoint = strings.Join(fields[3:], " ")
		}
		targets = append(targets, target)
	}
	return targets
}

// SelectTarget enforces single-device safety unless the caller explicitly asks for one ID.
func SelectTarget(targets []Target, requested string) (Target, error) {
	if requested != "" {
		for _, target := range targets {
			if target.ID != requested {
				continue
			}
			if !target.Usable() {
				return Target{}, apperror.New(
					apperror.CodeDeviceOffline,
					"the selected Harmony device is not Connected in hdc",
				)
			}
			return target, nil
		}
		return Target{}, apperror.New(
			apperror.CodeDeviceNotFound,
			"the requested Harmony device was not returned by hdc",
		)
	}

	var online []Target
	for _, target := range targets {
		if target.Usable() {
			online = append(online, target)
		}
	}
	switch len(online) {
	case 0:
		if len(targets) > 0 {
			return Target{}, apperror.New(
				apperror.CodeDeviceOffline,
				"a Harmony device is visible to hdc but is not Connected. Unlock or reconnect it and authorize debugging.",
			)
		}
		return Target{}, apperror.New(
			apperror.CodeNoDevice,
			"no Harmony device is connected. Connect one over USB and authorize debugging.",
		)
	case 1:
		return online[0], nil
	default:
		return Target{}, apperror.New(
			apperror.CodeMultipleDevices,
			"multiple Harmony devices are Connected. Select exactly one with --device.",
		)
	}
}

// AddReverse creates one device-to-Mac TCP mapping.
func (m *Manager) AddReverse(ctx context.Context, targetID string, mapping Mapping) error {
	if err := validateMapping(mapping); err != nil {
		return err
	}
	args := []string{"-t", targetID, "rport", "tcp:" + strconv.Itoa(mapping.DevicePort), "tcp:" + strconv.Itoa(mapping.HostPort)}
	output, err := m.runner().Run(ctx, m.Path, args...)
	if err != nil || !strings.Contains(output, "Forwardport result:OK") {
		return apperror.Wrap(
			apperror.CodeRPortFailed,
			"hdc could not create the reverse port mapping",
			commandFailure(err, output, targetID),
		)
	}
	return nil
}

// Remove removes the exact forward/reverse mapping tuple owned by this process.
func (m *Manager) Remove(ctx context.Context, targetID string, mapping Mapping) error {
	if err := validateMapping(mapping); err != nil {
		return err
	}
	args := []string{"-t", targetID, "fport", "rm", "tcp:" + strconv.Itoa(mapping.DevicePort), "tcp:" + strconv.Itoa(mapping.HostPort)}
	output, err := m.runner().Run(ctx, m.Path, args...)
	if err != nil {
		return fmt.Errorf("remove hdc mapping: %w", commandFailure(err, output, targetID))
	}
	if output != "" && !strings.Contains(strings.ToLower(output), "success") {
		return fmt.Errorf("remove hdc mapping: %s", sanitizeOutput(output, targetID))
	}
	return nil
}

func (m *Manager) runner() Runner {
	if m.Runner == nil {
		return ExecRunner{}
	}
	return m.Runner
}

func validateMapping(mapping Mapping) error {
	if mapping.DevicePort < 1 || mapping.DevicePort > 65_535 || mapping.HostPort < 1 || mapping.HostPort > 65_535 {
		return apperror.New(apperror.CodePortConflict, "the requested TCP port is outside 1...65535")
	}
	return nil
}

func commandFailure(err error, output, targetID string) error {
	safeOutput := sanitizeOutput(output, targetID)
	if err == nil {
		return fmt.Errorf("unexpected hdc output: %s", safeOutput)
	}
	if safeOutput == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, safeOutput)
}

func sanitizeOutput(output, targetID string) string {
	return strings.ReplaceAll(strings.TrimSpace(output), targetID, RedactTarget(targetID))
}

func isFailureOutput(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[Fail]") || (strings.HasPrefix(trimmed, "[E") && strings.Contains(trimmed, "]")) {
			return true
		}
	}
	return false
}

// RedactTarget converts an arbitrary hdc ID into a non-reversible short label.
func RedactTarget(targetID string) string {
	if targetID == "" {
		return "device-unknown"
	}
	digest := sha256.Sum256([]byte(targetID))
	return fmt.Sprintf("device-%x", digest[:4])
}
