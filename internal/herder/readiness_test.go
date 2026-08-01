package herder

import "testing"

func attached(networkName, ip, mac string) ContainerState {
	return ContainerState{
		Running:   true,
		Health:    "healthy",
		StartedAt: "2026-07-27T00:00:00Z",
		Networks:  map[string]Endpoint{networkName: {IPv4: ip, MAC: mac}},
	}
}

func TestReadyIPComesFromTheCallerNetworkEndpoint(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	ip, err := checkAttachment(plan, plan.Units[0], attached("net", "172.28.0.4", "02:aa:bb:cc:dd:ee"))
	if err != nil {
		t.Fatalf("checkAttachment: %v", err)
	}
	if ip != "172.28.0.4" {
		t.Fatalf("ip = %q, want the inspected endpoint address", ip)
	}
}

func TestMissingCallerNetworkFailsStartup(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	state := attached("bridge", "172.17.0.2", "")
	_, err := checkAttachment(plan, plan.Units[0], state)
	assertFailure(t, err, CodeContainerStartFailed, PhaseStart)
}

func TestExtraNetworkAttachmentFailsStartup(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	state := attached("net", "172.28.0.4", "")
	state.Networks["bridge"] = Endpoint{IPv4: "172.17.0.2"}
	_, err := checkAttachment(plan, plan.Units[0], state)
	assertFailure(t, err, CodeContainerStartFailed, PhaseStart)
}

func TestHostPortBindingFailsStartup(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	state := attached("net", "172.28.0.4", "")
	state.HostPortBindings = 1
	_, err := checkAttachment(plan, plan.Units[0], state)
	assertFailure(t, err, CodeContainerStartFailed, PhaseStart)
}

func TestMissingEndpointAddressFailsStartup(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	_, err := checkAttachment(plan, plan.Units[0], attached("net", "", ""))
	assertFailure(t, err, CodeContainerStartFailed, PhaseStart)
}

func TestEndpointMACMismatchFailsStartupWithItsOwnCode(t *testing.T) {
	plan := buildPlan(t, ids("UXGENT"), mappedConfig("UXGENT"), "")
	state := attached("net", "172.28.0.5", "02:ff:ff:ff:ff:ff")
	_, err := checkAttachment(plan, plan.Units[0], state)
	assertFailure(t, err, CodeEndpointMACMismatch, PhaseStart)
}

func TestEndpointMACIsComparedCanonically(t *testing.T) {
	plan := buildPlan(t, ids("UXGENT"), mappedConfig("UXGENT"), "")
	want := plan.Units[0].Devices[0].MAC
	state := attached("net", "172.28.0.5", upper(want))
	if _, err := checkAttachment(plan, plan.Units[0], state); err != nil {
		t.Fatalf("an uppercase endpoint MAC was treated as a mismatch: %v", err)
	}
}

// A synthetic batch stands for several devices behind one endpoint, so its
// endpoint MAC is Docker's business and must not be compared to any device.
func TestSyntheticEndpointMACIsNotChecked(t *testing.T) {
	plan := buildPlan(t, ids("USM8P", "UGW3"), RuntimeConfig{}, "public/emu:1.0.0")
	state := attached("net", "172.28.0.4", "02:99:99:99:99:99")
	if _, err := checkAttachment(plan, plan.Units[0], state); err != nil {
		t.Fatalf("checkAttachment: %v", err)
	}
}

func TestPostReadyExitIsDeviceExited(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	base := attached("net", "172.28.0.4", "")
	state := base
	state.Running = false
	state.ExitCode = 137
	assertFailure(t, checkRuntimeState(plan, plan.Units[0], base, state), CodeDeviceExited, PhaseRuntime)
}

func TestPostReadyUnhealthyIsDeviceUnhealthy(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	base := attached("net", "172.28.0.4", "")
	state := base
	state.Health = "unhealthy"
	assertFailure(t, checkRuntimeState(plan, plan.Units[0], base, state), CodeDeviceUnhealthy, PhaseRuntime)
}

func TestPostReadyRestartCountIsDeviceRestarted(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	base := attached("net", "172.28.0.4", "")
	state := base
	state.RestartCount = 1
	assertFailure(t, checkRuntimeState(plan, plan.Units[0], base, state), CodeDeviceRestarted, PhaseRuntime)
}

func TestPostReadyStartTimestampChangeIsDeviceRestarted(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	base := attached("net", "172.28.0.4", "")
	state := base
	state.StartedAt = "2026-07-27T00:00:09Z"
	assertFailure(t, checkRuntimeState(plan, plan.Units[0], base, state), CodeDeviceRestarted, PhaseRuntime)
}

func TestPostReadyNetworkLossIsNetworkDetached(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	base := attached("net", "172.28.0.4", "")
	state := attached("other", "172.29.0.4", "")
	assertFailure(t, checkRuntimeState(plan, plan.Units[0], base, state), CodeNetworkDetached, PhaseRuntime)
}

func TestPostReadyExtraNetworkIsNetworkDetached(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	base := attached("net", "172.28.0.4", "")
	state := attached("net", "172.28.0.4", "")
	state.Networks["extra"] = Endpoint{IPv4: "172.30.0.2"}
	assertFailure(t, checkRuntimeState(plan, plan.Units[0], base, state), CodeNetworkDetached, PhaseRuntime)
}

func TestHealthyRunningDeviceKeepsRunning(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	base := attached("net", "172.28.0.4", "")
	if err := checkRuntimeState(plan, plan.Units[0], base, base); err != nil {
		t.Fatalf("a healthy device failed the check: %v", err)
	}
}

// A container with no healthcheck reports an empty health string. The images
// were checked for a HEALTHCHECK at pull time, so an empty value here is a
// transient inspect, not a violation.
func TestEmptyHealthStringIsNotAFailure(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	base := attached("net", "172.28.0.4", "")
	state := base
	state.Health = ""
	if err := checkRuntimeState(plan, plan.Units[0], base, state); err != nil {
		t.Fatalf("an empty health string failed the check: %v", err)
	}
}

func upper(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - ('a' - 'A')
		}
	}
	return string(out)
}
