package hdc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/realskyrin/harmony-netbridge/internal/apperror"
)

func TestParseTargets(t *testing.T) {
	t.Parallel()
	output := strings.Join([]string{
		"usb-one USB Connected localhost",
		"usb-two USB Offline localhost",
		"192.0.2.2:5555 TCP Online 192.0.2.2",
	}, "\n")
	got := ParseTargets(output)
	want := []Target{
		{ID: "usb-one", Connection: "USB", Status: "Connected", Endpoint: "localhost"},
		{ID: "usb-two", Connection: "USB", Status: "Offline", Endpoint: "localhost"},
		{ID: "192.0.2.2:5555", Connection: "TCP", Status: "Online", Endpoint: "192.0.2.2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseTargets() = %#v, want %#v", got, want)
	}
}

func TestParseTargetsEmptyAndFallback(t *testing.T) {
	t.Parallel()
	if got := ParseTargets("[Empty]\n"); len(got) != 0 {
		t.Fatalf("empty output parsed as %#v", got)
	}
	got := ParseTargets("single-target\n")
	if len(got) != 1 || !got[0].Usable() || got[0].Status != "Connected" {
		t.Fatalf("fallback output parsed as %#v", got)
	}
}

func TestParseInstalledApplicationLabels(t *testing.T) {
	t.Parallel()
	output := `[
  {"bundleName":"com.example.mail","label":"Mail"},
  {"bundleName":"com.example.browser","label":"Browser"},
  {"bundleName":"com.example.mail","label":"Duplicate"},
  {"bundleName":"invalid","label":"Ignored"}
]`
	want := []InstalledApplication{
		{BundleName: "com.example.browser", Label: "Browser"},
		{BundleName: "com.example.mail", Label: "Mail"},
	}
	if got := ParseInstalledApplicationLabels(output); !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseInstalledApplicationLabels() = %#v, want %#v", got, want)
	}
	if got := ParseInstalledApplicationLabels("not-json"); len(got) != 0 {
		t.Fatalf("malformed label output parsed as %#v", got)
	}
}

func TestParseInstalledApplicationNames(t *testing.T) {
	t.Parallel()
	output := "ID: 100:\n\tcom.example.mail\n\tcom.example.browser\n\tinvalid\n"
	want := []InstalledApplication{
		{BundleName: "com.example.browser"},
		{BundleName: "com.example.mail"},
	}
	if got := ParseInstalledApplicationNames(output); !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseInstalledApplicationNames() = %#v, want %#v", got, want)
	}
}

func TestManagerListsInstalledApplicationsWithLabelFallback(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{outputs: []string{
		"dump failed",
		"ID: 100:\n\tcom.example.browser",
	}}
	manager := &Manager{Path: "/test/hdc", Runner: runner}
	applications, err := manager.ListInstalledApplications(context.Background(), "secret-device-id")
	if err != nil {
		t.Fatal(err)
	}
	if len(applications) != 1 || applications[0].BundleName != "com.example.browser" {
		t.Fatalf("applications = %#v", applications)
	}
	want := [][]string{
		{"-t", "secret-device-id", "shell", "bm", "dump", "-a", "-l"},
		{"-t", "secret-device-id", "shell", "bm", "dump", "-a"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestSelectTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		targets   []Target
		requested string
		wantID    string
		wantCode  apperror.Code
	}{
		{name: "one connected", targets: []Target{{ID: "one", Status: "Connected"}}, wantID: "one"},
		{name: "legacy online", targets: []Target{{ID: "one", Status: "Online"}}, wantID: "one"},
		{name: "explicit", targets: []Target{{ID: "one", Status: "Connected"}, {ID: "two", Status: "Connected"}}, requested: "two", wantID: "two"},
		{name: "none", wantCode: apperror.CodeNoDevice},
		{name: "offline", targets: []Target{{ID: "one", Status: "Offline"}}, wantCode: apperror.CodeDeviceOffline},
		{name: "multiple", targets: []Target{{ID: "one", Status: "Connected"}, {ID: "two", Status: "Connected"}}, wantCode: apperror.CodeMultipleDevices},
		{name: "explicit offline", targets: []Target{{ID: "one", Status: "Offline"}}, requested: "one", wantCode: apperror.CodeDeviceOffline},
		{name: "explicit missing", targets: []Target{{ID: "one", Status: "Connected"}}, requested: "two", wantCode: apperror.CodeDeviceNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := SelectTarget(test.targets, test.requested)
			if test.wantCode == "" {
				if err != nil || got.ID != test.wantID {
					t.Fatalf("SelectTarget() = %#v, %v; want ID %q", got, err, test.wantID)
				}
				return
			}
			var appErr *apperror.Error
			if !errors.As(err, &appErr) || appErr.Code != test.wantCode {
				t.Fatalf("error = %v, want code %s", err, test.wantCode)
			}
		})
	}
}

func TestManagerOwnsExactReverseMapping(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{outputs: []string{"Forwardport result:OK", "Remove forward ruler success"}}
	manager := &Manager{Path: "/test/hdc", Runner: runner}
	mapping := Mapping{DevicePort: 27183, HostPort: 54321}
	if err := manager.AddReverse(context.Background(), "secret-device-id", mapping); err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(context.Background(), "secret-device-id", mapping); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"-t", "secret-device-id", "rport", "tcp:27183", "tcp:54321"},
		{"-t", "secret-device-id", "fport", "rm", "tcp:27183", "tcp:54321"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestManagerRejectsFailureTextEvenWithZeroExitStatus(t *testing.T) {
	t.Parallel()
	manager := &Manager{
		Path:   "/test/hdc",
		Runner: &recordingRunner{outputs: []string{"[Fail] server unavailable", "[E001005] Device not connected"}},
	}
	if _, err := manager.Version(context.Background()); err == nil {
		t.Fatal("Version() accepted hdc failure text")
	}
	if _, err := manager.ListTargets(context.Background()); err == nil {
		t.Fatal("ListTargets() accepted hdc failure text")
	}
}

func TestAddReverseRedactsTargetInError(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{outputs: []string{"failed for secret-device-id"}, errors: []error{errors.New("exit 1")}}
	manager := &Manager{Path: "/test/hdc", Runner: runner}
	err := manager.AddReverse(context.Background(), "secret-device-id", Mapping{DevicePort: 27183, HostPort: 54321})
	if err == nil {
		t.Fatal("AddReverse returned nil error")
	}
	if strings.Contains(err.Error(), "secret-device-id") {
		t.Fatalf("error leaked target ID: %v", err)
	}
}

func TestRedactTargetStable(t *testing.T) {
	t.Parallel()
	first := RedactTarget("sensitive-id")
	if first != RedactTarget("sensitive-id") || first == RedactTarget("another-id") {
		t.Fatalf("unexpected redaction behavior: %q", first)
	}
}

type recordingRunner struct {
	calls   [][]string
	outputs []string
	errors  []error
}

func (r *recordingRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	index := len(r.calls) - 1
	var output string
	if index < len(r.outputs) {
		output = r.outputs[index]
	}
	var err error
	if index < len(r.errors) {
		err = r.errors[index]
	}
	return output, err
}
