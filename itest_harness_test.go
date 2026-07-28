package emu_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/testkit"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
)

const (
	defaultClassicImage = "ghcr.io/jamesbraid/unifi-network:sim"
	defaultUOSImage     = "ghcr.io/jamesbraid/unifi-os-server:seeded"
)

type itestImages struct {
	classic  string
	uos      string
	emulator string
}

func loadITestImages() itestImages {
	return itestImages{
		classic:  envOrDefault("UNIFI_EMU_ITEST_CLASSIC_IMAGE", defaultClassicImage),
		uos:      envOrDefault("UNIFI_EMU_ITEST_UOS_IMAGE", defaultUOSImage),
		emulator: os.Getenv("UNIFI_EMU_ITEST_EMULATOR_IMAGE"),
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func evidenceDir(testName string) string {
	return filepath.Join("tmp", "itest", testkit.NormalizeTestName(testName))
}

func discoverDockerHost(current, home string, socketExists func(string) bool) string {
	return testkit.DiscoverDockerHost(current, home, socketExists)
}

func containerRuntimeSocketOverride(host string) string {
	return testkit.ContainerRuntimeSocketOverride(host)
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func classicContainerRequest(networkName, image string) testcontainers.ContainerRequest {
	return testkit.BuildControllerRequest(networkName, image, false)
}

func uosContainerRequest(networkName, image string) testcontainers.ContainerRequest {
	return testkit.BuildControllerRequest(networkName, image, true)
}

const (
	itestMACBase = "00:27:22:e0:00:00"
	itestIPBase  = "192.168.1.100"
)

func emulatorModelsRequest(networkName string, images itestImages, informURL, modelsCSV string) testcontainers.ContainerRequest {
	cp := testkit.ContainerPlan{
		RuntimeName: "synthetic",
		Specs:       parseModelsCSV(modelsCSV),
	}
	cfg := testkit.HarnessConfig{
		EmulatorImage: images.emulator,
		GoArch:        runtime.GOARCH,
		ExtraEnv: map[string]string{
			"SIM_MODELS":   modelsCSV,
			"SIM_MAC_BASE": itestMACBase,
			"SIM_IP_BASE":  itestIPBase,
		},
	}
	req, err := testkit.BuildContainerRequest(cp, cfg, networkName, informURL)
	if err != nil {
		panic(err)
	}
	return req
}

func flagsExpress(spec emu.DeviceSpec) bool {
	return spec.FWCaps == nil && len(spec.SSIDs) == 0 && spec.Ports == 0
}

func emulatorContainerRequest(
	networkName string,
	images itestImages,
	informURL string,
	specs []emu.DeviceSpec,
) testcontainers.ContainerRequest {
	cp := testkit.ContainerPlan{
		RuntimeName: "synthetic",
		Specs:       specs,
	}
	cfg := testkit.HarnessConfig{
		EmulatorImage: images.emulator,
		GoArch:        runtime.GOARCH,
	}
	req, err := testkit.BuildContainerRequest(cp, cfg, networkName, informURL)
	if err != nil {
		panic(err)
	}
	return req
}

func emulatorAdoptRequest(
	networkName string,
	images itestImages,
	informURL string,
	specs []emu.DeviceSpec,
	adoptURL, user, password string,
) testcontainers.ContainerRequest {
	cp := testkit.ContainerPlan{
		RuntimeName: "synthetic",
		Specs:       specs,
	}
	cfg := testkit.HarnessConfig{
		EmulatorImage: images.emulator,
		GoArch:        runtime.GOARCH,
		ExtraEnv: map[string]string{
			"SIM_ADOPT":          "1",
			"SIM_ADOPT_URL":      adoptURL,
			"SIM_ADOPT_USERNAME": user,
			"SIM_ADOPT_PASSWORD": password,
		},
	}
	req, err := testkit.BuildContainerRequest(cp, cfg, networkName, informURL)
	if err != nil {
		panic(err)
	}
	return req
}

func emulatorBuildArgs(goarch string) map[string]*string {
	return testkit.EmulatorBuildArgs(goarch)
}

func applyUOSHostConfig(cfg *container.HostConfig) {
	tempReq := testkit.BuildControllerRequest("net", "image", true)
	tempReq.HostConfigModifier(cfg)
}

func TestLoadITestImagesUsesOverrides(t *testing.T) {
	t.Setenv("UNIFI_EMU_ITEST_CLASSIC_IMAGE", "example/classic:test")
	t.Setenv("UNIFI_EMU_ITEST_UOS_IMAGE", "example/uos:test")
	t.Setenv("UNIFI_EMU_ITEST_EMULATOR_IMAGE", "example/emu:test")

	got := loadITestImages()
	want := itestImages{
		classic:  "example/classic:test",
		uos:      "example/uos:test",
		emulator: "example/emu:test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadITestImages() = %#v, want %#v", got, want)
	}
}

func TestLoadITestImagesUsesDefaults(t *testing.T) {
	t.Setenv("UNIFI_EMU_ITEST_CLASSIC_IMAGE", "")
	t.Setenv("UNIFI_EMU_ITEST_UOS_IMAGE", "")
	t.Setenv("UNIFI_EMU_ITEST_EMULATOR_IMAGE", "")

	got := loadITestImages()
	if got.classic != "ghcr.io/jamesbraid/unifi-network:sim" {
		t.Fatalf("classic image = %q", got.classic)
	}
	if got.uos != "ghcr.io/jamesbraid/unifi-os-server:seeded" {
		t.Fatalf("UOS image = %q", got.uos)
	}
	if got.emulator != "" {
		t.Fatalf("emulator image = %q, want source build", got.emulator)
	}
}

func TestApplyUOSHostConfig(t *testing.T) {
	cfg := new(container.HostConfig)
	applyUOSHostConfig(cfg)

	if string(cfg.CgroupnsMode) != "host" {
		t.Fatalf("cgroup namespace = %q, want host", cfg.CgroupnsMode)
	}
	wantBinds := []string{"/sys/fs/cgroup:/sys/fs/cgroup:rw"}
	if !reflect.DeepEqual(cfg.Binds, wantBinds) {
		t.Fatalf("binds = %#v, want %#v", cfg.Binds, wantBinds)
	}
	wantDrop := []string{"ALL"}
	if !reflect.DeepEqual([]string(cfg.CapDrop), wantDrop) {
		t.Fatalf("cap drop = %#v, want %#v", cfg.CapDrop, wantDrop)
	}
	wantAdd := []string{
		"SYS_ADMIN", "NET_ADMIN", "NET_RAW", "NET_BIND_SERVICE",
		"DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "CHOWN",
		"SETUID", "SETGID", "KILL", "SYS_CHROOT", "SYS_PTRACE",
		"SYS_RESOURCE", "AUDIT_WRITE", "MKNOD",
	}
	if !reflect.DeepEqual([]string(cfg.CapAdd), wantAdd) {
		t.Fatalf("cap add = %#v, want %#v", cfg.CapAdd, wantAdd)
	}
	wantTmpfs := map[string]string{
		"/run":               "exec",
		"/run/lock":          "",
		"/tmp":               "exec",
		"/var/lib/journal":   "",
		"/var/opt/unifi/tmp": "size=64m",
	}
	if !reflect.DeepEqual(cfg.Tmpfs, wantTmpfs) {
		t.Fatalf("tmpfs = %#v, want %#v", cfg.Tmpfs, wantTmpfs)
	}
}

func parseModelsCSV(s string) []emu.DeviceSpec {
	var specs []emu.DeviceSpec
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		model, countText, hasCount := strings.Cut(item, ":")
		model = strings.TrimSpace(model)
		count := 1
		if hasCount {
			n, err := strconv.Atoi(strings.TrimSpace(countText))
			if err == nil && n > 0 {
				count = n
			}
		}
		for i := 0; i < count; i++ {
			specs = append(specs, emu.DeviceSpec{Model: model})
		}
	}
	return specs
}
