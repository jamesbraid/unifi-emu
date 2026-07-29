package herder

import (
	"encoding/json"
	"net"

	"github.com/moby/moby/api/types/container"
	networktypes "github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
)

// Public run labels. They support inspection and ownership checks; they do
// not perform cleanup, and they never carry an image reference or a routing
// decision, so a container listing tells an operator which run owns what
// without telling a public CI job which runtime ran.
const (
	labelHerder  = "io.unifi-emu.herder"
	labelRun     = "io.unifi-emu.herder.run"
	labelDevices = "io.unifi-emu.herder.devices"
)

// The complete device-container environment contract.
const (
	envInformURL   = "UNIFI_EMU_INFORM_URL"
	envDevicesJSON = "UNIFI_EMU_DEVICES_JSON"
)

// devicePayload is one entry of UNIFI_EMU_DEVICES_JSON. It deliberately omits
// the request index: a runtime is told who it is, not where it sat in a list.
type devicePayload struct {
	Model  string `json:"model"`
	MAC    string `json:"mac"`
	Serial string `json:"serial"`
	Name   string `json:"name"`
}

// buildContainerRequest renders one unit as a stock testcontainers request.
// Everything an operator could otherwise reach through -- commands, mounts,
// extra environment, privilege, host networking, published ports -- is absent
// by construction rather than filtered later.
func buildContainerRequest(plan Plan, unit Unit) testcontainers.ContainerRequest {
	devices := make([]devicePayload, len(unit.Devices))
	for i, d := range unit.Devices {
		devices[i] = devicePayload{Model: d.Model, MAC: d.MAC, Serial: d.Serial, Name: d.Name}
	}
	// Identities are already validated ASCII, so this cannot fail.
	payload, _ := json.Marshal(devices)

	capAdd := append([]string(nil), unit.CapAdd...)
	endpointMAC := unit.EndpointMAC()

	return testcontainers.ContainerRequest{
		Image: unit.Image,
		Env: map[string]string{
			envInformURL:   plan.InformURL,
			envDevicesJSON: string(payload),
		},
		Networks: []string{plan.Network},
		Labels: map[string]string{
			labelHerder:  "true",
			labelRun:     plan.RunID,
			labelDevices: unit.DeviceIndices(),
		},
		// No wait strategy here on purpose. Container.Start runs WaitingFor
		// in its post-start hook, so a strategy set here would report a
		// health failure as a creation failure -- container_start_failed in
		// phase start, where the contract requires device_unhealthy in phase
		// health. The backend waits on the same stock strategy afterwards.
		HostConfigModifier: func(cfg *container.HostConfig) {
			// Any restart is external interference. A restarted device has
			// lost its in-memory adoption key while still looking connected
			// to the controller, which is worse than a failed run.
			cfg.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyDisabled}
			cfg.Privileged = false
			cfg.PublishAllPorts = false
			cfg.PortBindings = nil
			cfg.Binds = nil
			cfg.Mounts = nil
			cfg.CapDrop = []string{"ALL"}
			cfg.CapAdd = capAdd
		},
		EndpointSettingsModifier: func(endpoints map[string]*networktypes.EndpointSettings) {
			if endpointMAC == "" {
				return
			}
			hw, err := net.ParseMAC(endpointMAC)
			if err != nil {
				return // unreachable: the MAC was canonicalized at allocation
			}
			for _, ep := range endpoints {
				ep.MacAddress = networktypes.HardwareAddr(hw)
			}
		},
	}
}
