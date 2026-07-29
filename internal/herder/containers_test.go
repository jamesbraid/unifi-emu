package herder

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

func TestSyntheticRequestCarriesEveryUnmappedDeviceInRequestOrder(t *testing.T) {
	plan := buildPlan(t, ids("USM8P", "UGW3"), RuntimeConfig{}, "public/emu:1.0.0")
	req := buildContainerRequest(plan, plan.Units[0])

	if req.Image != "public/emu:1.0.0" {
		t.Fatalf("image = %q", req.Image)
	}
	if got := req.Env["UNIFI_EMU_INFORM_URL"]; got != plan.InformURL {
		t.Fatalf("inform URL = %q, want %q", got, plan.InformURL)
	}
	var devices []map[string]string
	if err := json.Unmarshal([]byte(req.Env["UNIFI_EMU_DEVICES_JSON"]), &devices); err != nil {
		t.Fatalf("decode devices JSON: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("devices = %d, want both unmapped devices", len(devices))
	}
	if devices[0]["model"] != "USM8P" || devices[1]["model"] != "UGW3" {
		t.Fatalf("devices = %#v, want request order", devices)
	}
	for _, d := range devices {
		if len(d) != 4 {
			t.Fatalf("device payload = %#v, want exactly model, mac, serial and name", d)
		}
		for _, key := range []string{"model", "mac", "serial", "name"} {
			if _, ok := d[key]; !ok {
				t.Fatalf("device payload %#v has no %s", d, key)
			}
		}
	}
}

func TestOpaqueRequestCarriesExactlyOneDevice(t *testing.T) {
	plan := buildPlan(t, ids("UXGENT"), mappedConfig("UXGENT"), "")
	req := buildContainerRequest(plan, plan.Units[0])
	var devices []map[string]string
	if err := json.Unmarshal([]byte(req.Env["UNIFI_EMU_DEVICES_JSON"]), &devices); err != nil {
		t.Fatalf("decode devices JSON: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("devices = %d, want exactly one", len(devices))
	}
}

// The container contract is exactly two herder-defined variables. Anything
// else -- a runtime name, an image reference, operator environment -- would
// widen the surface an opaque runtime may depend on.
func TestRequestPassesOnlyTheTwoContractVariables(t *testing.T) {
	plan := buildPlan(t, ids("UXGENT"), mappedConfig("UXGENT"), "")
	req := buildContainerRequest(plan, plan.Units[0])
	want := []string{"UNIFI_EMU_DEVICES_JSON", "UNIFI_EMU_INFORM_URL"}
	got := make([]string, 0, len(req.Env))
	for k := range req.Env {
		got = append(got, k)
	}
	sortStrings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}

func TestRequestAttachesExactlyTheCallerNetwork(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	req := buildContainerRequest(plan, plan.Units[0])
	if !reflect.DeepEqual(req.Networks, []string{"net"}) {
		t.Fatalf("networks = %#v, want exactly the caller network", req.Networks)
	}
	if req.NetworkMode != "" {
		t.Fatalf("network mode = %q, want the default", req.NetworkMode)
	}
}

func TestRequestPublishesNoPorts(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	req := buildContainerRequest(plan, plan.Units[0])
	if len(req.ExposedPorts) != 0 {
		t.Fatalf("exposed ports = %#v, want none", req.ExposedPorts)
	}
	cfg := new(container.HostConfig)
	req.HostConfigModifier(cfg)
	if len(cfg.PortBindings) != 0 {
		t.Fatalf("port bindings = %#v, want none", cfg.PortBindings)
	}
	if cfg.PublishAllPorts {
		t.Fatal("publish-all-ports is set")
	}
}

func TestRequestGrantsNoHostAccess(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	req := buildContainerRequest(plan, plan.Units[0])
	if len(req.Cmd) != 0 || len(req.Entrypoint) != 0 {
		t.Fatalf("cmd = %#v entrypoint = %#v, want the image defaults", req.Cmd, req.Entrypoint)
	}
	if len(req.Mounts) != 0 || len(req.Files) != 0 || len(req.Tmpfs) != 0 {
		t.Fatal("the request carries mounts, files or tmpfs")
	}
	if req.Privileged {
		t.Fatal("the request is privileged")
	}
	cfg := new(container.HostConfig)
	req.HostConfigModifier(cfg)
	if cfg.Privileged {
		t.Fatal("the host config is privileged")
	}
	if len(cfg.Binds) != 0 || len(cfg.Mounts) != 0 {
		t.Fatalf("binds = %#v mounts = %#v, want none", cfg.Binds, cfg.Mounts)
	}
	if cfg.NetworkMode.IsHost() {
		t.Fatal("the host config uses host networking")
	}
}

// A restart would hide lost in-memory adoption keys behind a container that
// looks alive, so Docker is told never to restart and any restart is read as
// external interference.
func TestRequestDisablesTheDockerRestartPolicy(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	req := buildContainerRequest(plan, plan.Units[0])
	cfg := new(container.HostConfig)
	req.HostConfigModifier(cfg)
	if cfg.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Fatalf("restart policy = %q, want %q", cfg.RestartPolicy.Name, container.RestartPolicyDisabled)
	}
}

func TestOpaqueRequestDropsAllCapabilitiesThenAddsTheMappedSet(t *testing.T) {
	cfg := RuntimeConfig{Version: 1, Models: map[string]ModelRuntime{
		"UXGENT": {Image: "fixture.example/x@" + testDigest, CapAdd: []string{"NET_ADMIN", "NET_RAW"}},
	}}
	plan := buildPlan(t, ids("UXGENT"), cfg, "")
	req := buildContainerRequest(plan, plan.Units[0])
	host := new(container.HostConfig)
	req.HostConfigModifier(host)
	if !reflect.DeepEqual([]string(host.CapDrop), []string{"ALL"}) {
		t.Fatalf("cap drop = %#v, want ALL", host.CapDrop)
	}
	if !reflect.DeepEqual([]string(host.CapAdd), []string{"NET_ADMIN", "NET_RAW"}) {
		t.Fatalf("cap add = %#v", host.CapAdd)
	}
}

func TestSyntheticRequestAddsNoCapabilities(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	req := buildContainerRequest(plan, plan.Units[0])
	host := new(container.HostConfig)
	req.HostConfigModifier(host)
	if !reflect.DeepEqual([]string(host.CapDrop), []string{"ALL"}) {
		t.Fatalf("cap drop = %#v, want ALL", host.CapDrop)
	}
	if len(host.CapAdd) != 0 {
		t.Fatalf("cap add = %#v, want none", host.CapAdd)
	}
}

func TestOpaqueRequestAssignsTheRequestedEndpointMAC(t *testing.T) {
	plan := buildPlan(t, ids("UXGENT"), mappedConfig("UXGENT"), "")
	req := buildContainerRequest(plan, plan.Units[0])
	endpoints := map[string]*network.EndpointSettings{"net": {}}
	req.EndpointSettingsModifier(endpoints)
	got := endpoints["net"].MacAddress.String()
	if got != plan.Units[0].Devices[0].MAC {
		t.Fatalf("endpoint MAC = %q, want %q", got, plan.Units[0].Devices[0].MAC)
	}
}

func TestSyntheticRequestAssignsNoEndpointMAC(t *testing.T) {
	plan := buildPlan(t, ids("USM8P", "UGW3"), RuntimeConfig{}, "public/emu:1.0.0")
	req := buildContainerRequest(plan, plan.Units[0])
	endpoints := map[string]*network.EndpointSettings{"net": {}}
	req.EndpointSettingsModifier(endpoints)
	if len(endpoints["net"].MacAddress) != 0 {
		t.Fatalf("endpoint MAC = %q, want none", endpoints["net"].MacAddress)
	}
}

func TestRunLabelsCarryNoImageOrRoutingInformation(t *testing.T) {
	plan := buildPlan(t, ids("USM8P", "UXGENT"), mappedConfig("UXGENT"), "public/emu:1.0.0")
	for _, unit := range plan.Units {
		req := buildContainerRequest(plan, unit)
		if req.Labels[labelHerder] != "true" {
			t.Fatalf("label %s = %q", labelHerder, req.Labels[labelHerder])
		}
		if req.Labels[labelRun] != "run1" {
			t.Fatalf("label %s = %q", labelRun, req.Labels[labelRun])
		}
		if req.Labels[labelDevices] != unit.DeviceIndices() {
			t.Fatalf("label %s = %q, want %q", labelDevices, req.Labels[labelDevices], unit.DeviceIndices())
		}
		for key, value := range req.Labels {
			if strings.Contains(value, unit.Image) {
				t.Fatalf("label %s leaks the image reference", key)
			}
		}
	}
}

// The request must carry no wait strategy. Container.Start runs WaitingFor in
// its post-start hook, so a strategy here would make a health failure surface
// as a creation failure -- container_start_failed in phase start, where the
// contract requires device_unhealthy in phase health. The health wait is the
// backend's own step instead.
func TestRequestSetsNoCreationTimeWaitStrategy(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	req := buildContainerRequest(plan, plan.Units[0])
	if req.WaitingFor != nil {
		t.Fatalf("wait strategy = %T, want none at creation time", req.WaitingFor)
	}
}

// Readiness still comes from Docker health state alone: the herder never parses
// container logs for it, so the strategy it waits on must be the stock
// healthcheck one.
func TestHealthWaitUsesTheDockerHealthcheckStrategy(t *testing.T) {
	got := strings.ToLower(reflect.TypeOf(healthWait()).String())
	if !strings.Contains(got, "health") {
		t.Fatalf("health wait strategy = %s, want a health-check strategy", got)
	}
}

// The herder must not customize the Testcontainers session: sibling herders
// under one parent are meant to share the stock session so crash cleanup
// behaves as the library documents.
func TestRequestSetsNoTestcontainersSessionLabel(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	req := buildContainerRequest(plan, plan.Units[0])
	for key := range req.Labels {
		if strings.Contains(key, "sessionId") || strings.Contains(key, "session-id") {
			t.Fatalf("request pins a Testcontainers session via %s", key)
		}
	}
	if req.SkipReaper {
		t.Fatal("the request skips the reaper")
	}
	if len(req.ReaperOptions) != 0 || req.ReaperImage != "" {
		t.Fatal("the request customizes the reaper")
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
