package emu_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
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
	var normalized strings.Builder
	underscore := false
	for _, r := range strings.ToLower(testName) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			normalized.WriteRune(r)
			underscore = false
			continue
		}
		if !underscore && normalized.Len() > 0 {
			normalized.WriteByte('_')
			underscore = true
		}
	}
	return filepath.Join("tmp", "itest", strings.Trim(normalized.String(), "_"))
}

func discoverDockerHost(current, home string, socketExists func(string) bool) string {
	if current != "" {
		return current
	}
	colima := filepath.Join(home, ".colima", "default", "docker.sock")
	if socketExists(colima) {
		return "unix://" + colima
	}
	return ""
}

func containerRuntimeSocketOverride(host string) string {
	if strings.Contains(host, "/.colima/") {
		return "/var/run/docker.sock"
	}
	return ""
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
	return testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{"8443/tcp"},
		Networks:     []string{networkName},
		NetworkAliases: map[string][]string{
			networkName: {"controller"},
		},
		WaitingFor: wait.ForHealthCheck().
			WithPollInterval(time.Second).
			WithStartupTimeout(5 * time.Minute),
	}
}

func uosContainerRequest(networkName, image string) testcontainers.ContainerRequest {
	return testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{"443/tcp"},
		Networks:     []string{networkName},
		NetworkAliases: map[string][]string{
			networkName: {"controller"},
		},
		WaitingFor: wait.ForHealthCheck().
			WithPollInterval(time.Second).
			WithStartupTimeout(10 * time.Minute),
		HostConfigModifier: applyUOSHostConfig,
	}
}

// baseEmulatorRequest builds the container request common to every launch
// mode: the shared network, the -inform command, and the emulator image
// (built from the checkout unless a prebuilt one is selected).
func baseEmulatorRequest(networkName string, images itestImages, informURL string) testcontainers.ContainerRequest {
	request := testcontainers.ContainerRequest{
		Networks: []string{networkName},
		Cmd:      []string{"-inform", informURL},
	}
	if images.emulator == "" {
		request.FromDockerfile = testcontainers.FromDockerfile{
			Context:    ".",
			Dockerfile: "Dockerfile",
			Repo:       "unifi-emu-itest",
			Tag:        strings.ReplaceAll(networkName, "_", "-"),
			BuildArgs:  emulatorBuildArgs(runtime.GOARCH),
		}
	} else {
		request.Image = images.emulator
	}
	return request
}

// itestMACBase and itestIPBase pin the emulator's SIM_MODELS expansion so the
// caller can predict each device's MAC (base + index + 1). They match the
// emulator's own defaults but are set explicitly to keep the contract local.
const (
	itestMACBase = "00:27:22:e0:00:00"
	itestIPBase  = "192.168.1.100"
)

// emulatorModelsRequest launches the emulator from a terse SIM_MODELS list,
// exercising the CLI's model-list selector and MAC/IP auto-expansion end to
// end. SIM_DEVICES would bypass both.
func emulatorModelsRequest(networkName string, images itestImages, informURL, modelsCSV string) testcontainers.ContainerRequest {
	request := baseEmulatorRequest(networkName, images, informURL)
	request.Env = map[string]string{
		"SIM_MODELS":   modelsCSV,
		"SIM_MAC_BASE": itestMACBase,
		"SIM_IP_BASE":  itestIPBase,
	}
	return request
}

func emulatorContainerRequest(
	networkName string,
	images itestImages,
	informURL string,
	specs []emu.DeviceSpec,
) testcontainers.ContainerRequest {
	request := baseEmulatorRequest(networkName, images, informURL)

	if len(specs) == 1 {
		spec := specs[0]
		request.Cmd = append(request.Cmd,
			"-mac", spec.MAC,
			"-model", spec.Model,
			"-ip", spec.IP,
		)
		if spec.Type != "" {
			request.Cmd = append(request.Cmd, "-type", spec.Type)
		}
		if spec.ModelDisplay != "" {
			request.Cmd = append(request.Cmd, "-model-display", spec.ModelDisplay)
		}
		if spec.Version != "" {
			request.Cmd = append(request.Cmd, "-version", spec.Version)
		}
		if spec.Name != "" {
			request.Cmd = append(request.Cmd, "-name", spec.Name)
		}
		return request
	}

	encoded, err := json.Marshal(specs)
	if err != nil {
		panic("marshal static integration device specs: " + err.Error())
	}
	request.Env = map[string]string{"SIM_DEVICES": string(encoded)}
	return request
}

// emulatorAdoptRequest launches the emulator with its own adoption switched
// on, so the container drives the fleet to connected and the test issues no
// adopt of its own.
//
// adoptURL is the controller's API port on the shared container network —
// a different port from the inform URL, and the one thing adoption needs
// that informing does not.
func emulatorAdoptRequest(
	networkName string,
	images itestImages,
	informURL string,
	specs []emu.DeviceSpec,
	adoptURL, user, password string,
) testcontainers.ContainerRequest {
	request := emulatorContainerRequest(networkName, images, informURL, specs)
	// A single-device fleet rides on the command line and leaves Env unset;
	// a multi-device one already holds SIM_DEVICES, which must survive.
	if request.Env == nil {
		request.Env = map[string]string{}
	}
	request.Env["SIM_ADOPT"] = "1"
	request.Env["SIM_ADOPT_URL"] = adoptURL
	request.Env["SIM_ADOPT_USERNAME"] = user
	request.Env["SIM_ADOPT_PASSWORD"] = password
	return request
}

func emulatorBuildArgs(goarch string) map[string]*string {
	buildPlatform := "linux/" + goarch
	targetOS := "linux"
	targetArch := goarch
	return map[string]*string{
		"BUILDPLATFORM": &buildPlatform,
		"TARGETOS":      &targetOS,
		"TARGETARCH":    &targetArch,
	}
}

func applyUOSHostConfig(cfg *container.HostConfig) {
	cfg.CgroupnsMode = container.CgroupnsModeHost
	cfg.Binds = []string{"/sys/fs/cgroup:/sys/fs/cgroup:rw"}
	cfg.CapDrop = []string{"ALL"}
	cfg.CapAdd = []string{
		"SYS_ADMIN", "NET_ADMIN", "NET_RAW", "NET_BIND_SERVICE",
		"DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "CHOWN",
		"SETUID", "SETGID", "KILL", "SYS_CHROOT", "SYS_PTRACE",
		"SYS_RESOURCE", "AUDIT_WRITE", "MKNOD",
	}
	cfg.Tmpfs = map[string]string{
		"/run":               "exec",
		"/run/lock":          "",
		"/tmp":               "exec",
		"/var/lib/journal":   "",
		"/var/opt/unifi/tmp": "size=64m",
	}
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
