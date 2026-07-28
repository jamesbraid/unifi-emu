package herder

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// specMatrix is the protocol-1 code/phase/exit table transcribed from the
// design document. It is duplicated here on purpose: the test is the copy of
// the spec, the package is the implementation, and a drift between them is
// exactly what this asserts.
var specMatrix = []struct {
	code   Code
	phases []Phase
	exit   int
}{
	{CodeInvalidRequest, []Phase{PhaseValidate}, 1},
	{CodeInvalidRuntimeConfig, []Phase{PhaseValidate}, 1},
	{CodeNetworkNotFound, []Phase{PhaseValidate}, 1},
	{CodeInformURLInvalid, []Phase{PhaseValidate}, 1},
	{CodeDockerVersionUnsupported, []Phase{PhaseValidate}, 1},
	{CodeReaperDisabled, []Phase{PhaseValidate}, 1},
	{CodeSyntheticModelUnknown, []Phase{PhaseValidate}, 1},
	{CodeSyntheticImageRequired, []Phase{PhaseValidate}, 1},
	{CodeImagePullFailed, []Phase{PhasePull}, 1},
	{CodeStartupTimeout, []Phase{PhaseValidate, PhasePull, PhaseStart, PhaseHealth}, 1},
	{CodeImageInvalid, []Phase{PhasePull}, 1},
	{CodeCapabilityUnsupported, []Phase{PhaseValidate}, 1},
	{CodeContainerStartFailed, []Phase{PhaseStart}, 1},
	{CodeEndpointMACMismatch, []Phase{PhaseStart}, 1},
	{CodeDeviceUnhealthy, []Phase{PhaseHealth, PhaseRuntime}, 1},
	{CodeDeviceExited, []Phase{PhaseHealth, PhaseRuntime}, 1},
	{CodeDeviceRestarted, []Phase{PhaseRuntime}, 1},
	{CodeNetworkDetached, []Phase{PhaseRuntime}, 1},
	{CodeDockerUnavailable, []Phase{PhaseValidate, PhasePull, PhaseStart, PhaseHealth, PhaseRuntime}, 1},
	{CodeCleanupFailed, []Phase{PhaseCleanup}, 1},
}

var allPhases = []Phase{
	PhaseValidate, PhasePull, PhaseStart, PhaseHealth, PhaseRuntime, PhaseCleanup,
}

func TestCodePhaseExitMatrixMatchesSpec(t *testing.T) {
	if len(Codes()) != len(specMatrix) {
		t.Fatalf("Codes() has %d entries, spec matrix has %d", len(Codes()), len(specMatrix))
	}
	for _, want := range specMatrix {
		for _, phase := range allPhases {
			allowed := false
			for _, p := range want.phases {
				if p == phase {
					allowed = true
				}
			}
			if got := want.code.AllowsPhase(phase); got != allowed {
				t.Errorf("%s.AllowsPhase(%s) = %v, want %v", want.code, phase, got, allowed)
			}
		}
		if got := want.code.ExitStatus(); got != want.exit {
			t.Errorf("%s.ExitStatus() = %d, want %d", want.code, got, want.exit)
		}
		if want.code.Message() == "" {
			t.Errorf("%s has no stable public message", want.code)
		}
	}
}

func TestStartedEventEncoding(t *testing.T) {
	var buf strings.Builder
	w := NewWriter(&buf)
	if err := w.Started("7f8b1ab4"); err != nil {
		t.Fatalf("Started: %v", err)
	}
	want := `{"protocol":1,"event":"started","run_id":"7f8b1ab4"}` + "\n"
	if buf.String() != want {
		t.Fatalf("started event = %q, want %q", buf.String(), want)
	}
}

func TestReadyEventCarriesRequiredDeviceIP(t *testing.T) {
	var buf strings.Builder
	w := NewWriter(&buf)
	mustStart(t, w)
	err := w.Ready("7f8b1ab4", []ReadyDevice{{
		Identity: Identity{Index: 0, Model: "USM8P", MAC: "02:11:22:33:44:55",
			Serial: "EMU021122334455", Name: "emu-usm8p-021122334455"},
		IP: "172.28.0.4",
	}})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	want := `{"protocol":1,"event":"ready","run_id":"7f8b1ab4","devices":` +
		`[{"index":0,"model":"USM8P","mac":"02:11:22:33:44:55","serial":"EMU021122334455",` +
		`"name":"emu-usm8p-021122334455","ip":"172.28.0.4"}]}` + "\n"
	if got := lastLine(buf.String()); got != want {
		t.Fatalf("ready event = %q, want %q", got, want)
	}
}

func TestFailedEventOmitsDeviceIPAndCause(t *testing.T) {
	var buf strings.Builder
	w := NewWriter(&buf)
	mustStart(t, w)
	err := w.Failed(FailedEvent{
		RunID: "7f8b1ab4", Phase: PhaseHealth, Code: CodeDeviceUnhealthy,
		CleanupComplete: true,
		Devices: []Identity{{Index: 1, Model: "UXGENT", MAC: "02:00:00:00:10:01",
			Serial: "EMU020000001001", Name: "gateway-under-test"}},
	})
	if err != nil {
		t.Fatalf("Failed: %v", err)
	}
	line := lastLine(buf.String())
	if strings.Contains(line, `"ip"`) {
		t.Fatalf("failed event carries a device ip: %s", line)
	}
	if strings.Contains(line, `"cause"`) {
		t.Fatalf("failed event carries an empty cause: %s", line)
	}
	want := `{"protocol":1,"event":"failed","run_id":"7f8b1ab4","phase":"health",` +
		`"code":"device_unhealthy","message":"` + CodeDeviceUnhealthy.Message() + `",` +
		`"cleanup_complete":true,"devices":[{"index":1,"model":"UXGENT",` +
		`"mac":"02:00:00:00:10:01","serial":"EMU020000001001","name":"gateway-under-test"}]}` + "\n"
	if line != want {
		t.Fatalf("failed event = %q, want %q", line, want)
	}
}

func TestFailedCleanupEventCarriesCause(t *testing.T) {
	var buf strings.Builder
	w := NewWriter(&buf)
	mustStart(t, w)
	err := w.Failed(FailedEvent{
		RunID: "7f8b1ab4", Phase: PhaseCleanup, Code: CodeCleanupFailed,
		Cause: CodeDeviceUnhealthy,
	})
	if err != nil {
		t.Fatalf("Failed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lastLine(buf.String())), &got); err != nil {
		t.Fatalf("decode failed event: %v", err)
	}
	if got["cause"] != "device_unhealthy" {
		t.Fatalf("cause = %v, want device_unhealthy", got["cause"])
	}
	if got["cleanup_complete"] != false {
		t.Fatalf("cleanup_complete = %v, want false", got["cleanup_complete"])
	}
	if devices, ok := got["devices"].([]any); !ok || len(devices) != 0 {
		t.Fatalf("devices = %#v, want an empty array", got["devices"])
	}
}

func TestFailedEventRejectsPhaseOutsideMatrix(t *testing.T) {
	var buf strings.Builder
	w := NewWriter(&buf)
	mustStart(t, w)
	err := w.Failed(FailedEvent{RunID: "r", Phase: PhaseRuntime, Code: CodeInvalidRequest})
	if err == nil {
		t.Fatal("emitting invalid_request in phase runtime succeeded, want a rejection")
	}
	if strings.Contains(buf.String(), "failed") {
		t.Fatalf("rejected event still reached stdout: %q", buf.String())
	}
}

func TestOnlyOneTerminalEventIsEmitted(t *testing.T) {
	var buf strings.Builder
	w := NewWriter(&buf)
	mustStart(t, w)
	if err := w.Stopped("r", "signal"); err != nil {
		t.Fatalf("Stopped: %v", err)
	}
	if err := w.Failed(FailedEvent{RunID: "r", Phase: PhaseCleanup, Code: CodeCleanupFailed}); err == nil {
		t.Fatal("a second terminal event was accepted")
	}
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Fatalf("wrote %d events, want started + one terminal", n)
	}
}

func TestReadyRejectedAfterTerminalEvent(t *testing.T) {
	var buf strings.Builder
	w := NewWriter(&buf)
	mustStart(t, w)
	if err := w.Stopped("r", "signal"); err != nil {
		t.Fatalf("Stopped: %v", err)
	}
	if err := w.Ready("r", nil); err == nil {
		t.Fatal("ready after a terminal event was accepted")
	}
}

func TestStartedEmittedOnlyOnce(t *testing.T) {
	var buf strings.Builder
	w := NewWriter(&buf)
	mustStart(t, w)
	if err := w.Started("r"); err == nil {
		t.Fatal("a second started event was accepted")
	}
}

// brokenWriter fails every write, standing in for a closed stdout pipe.
type brokenWriter struct{ writes int }

func (b *brokenWriter) Write(p []byte) (int, error) {
	b.writes++
	return 0, errors.New("broken pipe")
}

func TestBrokenStdoutIsStickyAndStopsWriting(t *testing.T) {
	broken := &brokenWriter{}
	w := NewWriter(broken)
	if err := w.Started("r"); err == nil {
		t.Fatal("Started on a broken stdout returned no error")
	}
	if !w.Broken() {
		t.Fatal("Broken() = false after a failed write")
	}
	if err := w.Failed(FailedEvent{RunID: "r", Phase: PhaseStart, Code: CodeContainerStartFailed}); err == nil {
		t.Fatal("Failed on a broken stdout returned no error")
	}
	if broken.writes != 1 {
		t.Fatalf("writer attempted %d writes, want 1 before latching broken", broken.writes)
	}
}

func TestStoppedEventEncoding(t *testing.T) {
	var buf strings.Builder
	w := NewWriter(&buf)
	mustStart(t, w)
	if err := w.Stopped("7f8b1ab4", "signal"); err != nil {
		t.Fatalf("Stopped: %v", err)
	}
	want := `{"protocol":1,"event":"stopped","run_id":"7f8b1ab4","reason":"signal"}` + "\n"
	if got := lastLine(buf.String()); got != want {
		t.Fatalf("stopped event = %q, want %q", got, want)
	}
}

// TestEventsCarryNoRuntimeRouting guards the privacy boundary: no encoded
// event may name an image, registry or runtime mapping.
func TestEventsCarryNoRuntimeRouting(t *testing.T) {
	forbidden := []string{"image", "registry", "runtime", "container_id", "opaque"}
	var buf strings.Builder
	w := NewWriter(&buf)
	mustStart(t, w)
	if err := w.Ready("r", []ReadyDevice{{Identity: Identity{Index: 0, Model: "USM8P"}, IP: "172.28.0.4"}}); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if err := w.Failed(FailedEvent{RunID: "r", Phase: PhaseStart, Code: CodeContainerStartFailed}); err != nil {
		t.Fatalf("Failed: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		for key := range obj {
			for _, bad := range forbidden {
				if strings.Contains(key, bad) {
					t.Errorf("event key %q leaks runtime routing", key)
				}
			}
		}
	}
}

func TestReadyDeviceFieldOrderIsStable(t *testing.T) {
	body, err := json.Marshal(ReadyDevice{
		Identity: Identity{Index: 3, Model: "M", MAC: "m", Serial: "s", Name: "n"},
		IP:       "1.2.3.4",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"index":3,"model":"M","mac":"m","serial":"s","name":"n","ip":"1.2.3.4"}`
	if string(body) != want {
		t.Fatalf("ReadyDevice = %s, want %s", body, want)
	}
}

func TestFailureCarriesPublicMessageAndPrivateDetail(t *testing.T) {
	f := failf(CodeImagePullFailed, PhasePull, "pull %s: connection refused to registry.example", "img")
	if f.Message() != CodeImagePullFailed.Message() {
		t.Fatalf("public message = %q, want the stable code summary", f.Message())
	}
	if !strings.Contains(f.Error(), "registry.example") {
		t.Fatalf("private detail lost the cause: %q", f.Error())
	}
	var target *Failure
	if !errors.As(error(f), &target) {
		t.Fatal("Failure does not satisfy errors.As")
	}
}

func TestCodesAreUnique(t *testing.T) {
	seen := map[Code]bool{}
	for _, c := range Codes() {
		if seen[c] {
			t.Fatalf("duplicate code %s", c)
		}
		seen[c] = true
	}
	if !reflect.DeepEqual(len(seen), len(Codes())) {
		t.Fatal("Codes() contains duplicates")
	}
}

func mustStart(t *testing.T, w *Writer) {
	t.Helper()
	if err := w.Started("r"); err != nil {
		t.Fatalf("Started: %v", err)
	}
}

func lastLine(s string) string {
	lines := strings.SplitAfter(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}
