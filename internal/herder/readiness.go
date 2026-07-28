package herder

import "strings"

// Endpoint is one container network attachment as Docker reports it.
type Endpoint struct {
	IPv4 string
	MAC  string
}

// ContainerState is the inspected state the herder acts on. It is a reduced
// view of the Docker inspect response so the state machine can be exercised
// without a daemon.
type ContainerState struct {
	Running          bool
	Health           string // "", "starting", "healthy", "unhealthy"
	ExitCode         int
	RestartCount     int
	StartedAt        string
	Networks         map[string]Endpoint
	HostPortBindings int
}

// checkAttachment verifies one started container against the topology the
// contract fixes, and returns the address the ready event will report.
//
// The address comes from inspection, never from anything a child process
// printed: the herder publishes what Docker actually assigned.
func checkAttachment(plan Plan, unit Unit, state ContainerState) (string, error) {
	if len(state.Networks) != 1 {
		return "", failf(CodeContainerStartFailed, PhaseStart,
			"unit %s is attached to %d networks, want only %s",
			unit.DeviceIndices(), len(state.Networks), plan.Network)
	}
	endpoint, ok := state.Networks[plan.Network]
	if !ok {
		return "", failf(CodeContainerStartFailed, PhaseStart,
			"unit %s is not attached to %s", unit.DeviceIndices(), plan.Network)
	}
	// The herder requests no exposed or published ports, so a binding here
	// means something in the image or daemon added one behind its back.
	if state.HostPortBindings > 0 {
		return "", failf(CodeContainerStartFailed, PhaseStart,
			"unit %s has %d host port binding(s), want none",
			unit.DeviceIndices(), state.HostPortBindings)
	}
	if want := unit.EndpointMAC(); want != "" && !sameMAC(endpoint.MAC, want) {
		return "", failf(CodeEndpointMACMismatch, PhaseStart,
			"unit %s endpoint MAC is %q, want %q", unit.DeviceIndices(), endpoint.MAC, want)
	}
	if endpoint.IPv4 == "" {
		return "", failf(CodeContainerStartFailed, PhaseStart,
			"unit %s has no IPv4 address on %s", unit.DeviceIndices(), plan.Network)
	}
	return endpoint.IPv4, nil
}

// checkRuntimeState is the post-ready monitor's verdict on one container.
// Every violation fails the whole fleet: the herder never restarts a device,
// because a restarted device looks connected to the controller while having
// lost the adoption key the test is measuring.
func checkRuntimeState(plan Plan, unit Unit, baseline, state ContainerState) error {
	if !state.Running {
		return failf(CodeDeviceExited, PhaseRuntime,
			"unit %s exited with status %d", unit.DeviceIndices(), state.ExitCode)
	}
	if state.Health == "unhealthy" {
		return failf(CodeDeviceUnhealthy, PhaseRuntime,
			"unit %s reported unhealthy", unit.DeviceIndices())
	}
	if state.RestartCount != baseline.RestartCount || state.StartedAt != baseline.StartedAt {
		return failf(CodeDeviceRestarted, PhaseRuntime,
			"unit %s restarted (count %d, started %s)",
			unit.DeviceIndices(), state.RestartCount, state.StartedAt)
	}
	if len(state.Networks) != 1 {
		return failf(CodeNetworkDetached, PhaseRuntime,
			"unit %s is attached to %d networks, want only %s",
			unit.DeviceIndices(), len(state.Networks), plan.Network)
	}
	if _, ok := state.Networks[plan.Network]; !ok {
		return failf(CodeNetworkDetached, PhaseRuntime,
			"unit %s lost its attachment to %s", unit.DeviceIndices(), plan.Network)
	}
	return nil
}

// sameMAC compares two MAC spellings case-insensitively; Docker echoes back
// what it was given, and a case difference is not a mismatch.
func sameMAC(got, want string) bool { return strings.EqualFold(got, want) }
