package emu_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/internal/adopt"
)

func TestEvidenceDirUsesSafeTestName(t *testing.T) {
	got := evidenceDir("TestClassic/Fleet with spaces")
	want := filepath.Join("tmp", "itest", "testclassic_fleet_with_spaces")
	if got != want {
		t.Fatalf("evidenceDir() = %q, want %q", got, want)
	}
}

func TestWriteJSONPreservesDeviceDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending-devices.json")
	devices := []adopt.Device{{
		MAC:     "00:27:22:e0:00:01",
		State:   2,
		Adopted: false,
		Model:   "UGW3",
	}}
	if err := writeJSON(path, devices); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"mac": "00:27:22:e0:00:01"`,
		`"state": 2`,
		`"adopted": false`,
		`"model": "UGW3"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("%s does not contain %q:\n%s", path, want, body)
		}
	}
}

// TestEmulatorAdoptRequestKeepsTheFleet guards the merge: the adopt
// settings and the fleet definition share one Env map, and dropping
// SIM_DEVICES would boot the image's baked default fleet instead of the
// devices the test asked for — which still adopts, so the test would pass
// while proving nothing.
func TestEmulatorAdoptRequestKeepsTheFleet(t *testing.T) {
	specs := []emu.DeviceSpec{
		{MAC: "00:27:22:e0:00:41", Model: "U7PRO", IP: "192.168.1.181"},
		{MAC: "00:27:22:e0:00:42", Model: "USM8P", IP: "192.168.1.182"},
	}
	request := emulatorAdoptRequest(
		"itest-network", itestImages{emulator: "example/emu:test"},
		"http://10.0.0.2:8080/inform", specs,
		"https://10.0.0.2:8443", "admin", "s3cret",
	)

	if got := request.Env["SIM_DEVICES"]; !strings.Contains(got, "00:27:22:e0:00:41") {
		t.Errorf("SIM_DEVICES = %q, want the requested fleet", got)
	}
	for key, want := range map[string]string{
		"SIM_ADOPT":          "1",
		"SIM_ADOPT_URL":      "https://10.0.0.2:8443",
		"SIM_ADOPT_USERNAME": "admin",
		"SIM_ADOPT_PASSWORD": "s3cret",
	} {
		if got := request.Env[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestEmulatorAdoptRequestSingleDevice covers the other branch: one device
// is configured by command-line flags, leaving the Env map nil until the
// adopt settings create it.
func TestEmulatorAdoptRequestSingleDevice(t *testing.T) {
	specs := []emu.DeviceSpec{{MAC: "00:27:22:e0:00:41", Model: "U7PRO", IP: "192.168.1.181"}}
	request := emulatorAdoptRequest(
		"itest-network", itestImages{emulator: "example/emu:test"},
		"http://10.0.0.2:8080/inform", specs,
		"https://10.0.0.2:8443", "admin", "admin",
	)
	if request.Env["SIM_ADOPT"] != "1" {
		t.Errorf("SIM_ADOPT = %q, want 1", request.Env["SIM_ADOPT"])
	}
	if !slices.Contains(request.Cmd, "00:27:22:e0:00:41") {
		t.Errorf("command %v does not carry the device MAC", request.Cmd)
	}
}

func TestDiscoverDockerHostPreservesExplicitHost(t *testing.T) {
	got := discoverDockerHost(
		"unix:///explicit/docker.sock",
		"/Users/test",
		func(string) bool { return true },
	)
	if got != "unix:///explicit/docker.sock" {
		t.Fatalf("discoverDockerHost() = %q", got)
	}
}

func TestDiscoverDockerHostFindsColima(t *testing.T) {
	home := "/Users/test"
	colima := filepath.Join(home, ".colima", "default", "docker.sock")
	got := discoverDockerHost("", home, func(path string) bool {
		return path == colima
	})
	want := "unix://" + colima
	if got != want {
		t.Fatalf("discoverDockerHost() = %q, want %q", got, want)
	}
}

func TestContainerRuntimeSocketOverrideForColima(t *testing.T) {
	host := "unix:///Users/test/.colima/default/docker.sock"
	if got := containerRuntimeSocketOverride(host); got != "/var/run/docker.sock" {
		t.Fatalf("containerRuntimeSocketOverride() = %q", got)
	}
}

func TestContainerRuntimeSocketOverrideLeavesOtherHostsAlone(t *testing.T) {
	if got := containerRuntimeSocketOverride("unix:///var/run/docker.sock"); got != "" {
		t.Fatalf("containerRuntimeSocketOverride() = %q, want no override", got)
	}
}

func TestEmulatorBuildArgsProvideDockerfilePlatforms(t *testing.T) {
	args := emulatorBuildArgs("arm64")
	want := map[string]string{
		"BUILDPLATFORM": "linux/arm64",
		"TARGETOS":      "linux",
		"TARGETARCH":    "arm64",
	}
	for name, value := range want {
		if args[name] == nil || *args[name] != value {
			t.Fatalf("build arg %s = %v, want %q", name, args[name], value)
		}
	}
}

func TestDockerfileDeclaresBuildPlatformBeforeFirstFrom(t *testing.T) {
	body, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	arg := strings.Index(string(body), "ARG BUILDPLATFORM")
	from := strings.Index(string(body), "FROM --platform=$BUILDPLATFORM")
	if arg < 0 || from < 0 || arg > from {
		t.Fatalf("Dockerfile must declare ARG BUILDPLATFORM before its first parameterized FROM")
	}
}

func TestClassicContainerRequest(t *testing.T) {
	request := classicContainerRequest("itest-network", "example/classic:test")
	if request.Image != "example/classic:test" {
		t.Fatalf("image = %q", request.Image)
	}
	if !reflect.DeepEqual(request.Networks, []string{"itest-network"}) {
		t.Fatalf("networks = %#v", request.Networks)
	}
	if !reflect.DeepEqual(request.ExposedPorts, []string{"8443/tcp"}) {
		t.Fatalf("exposed ports = %#v", request.ExposedPorts)
	}
	if request.WaitingFor == nil {
		t.Fatal("classic request has no health wait strategy")
	}
}

func TestUOSContainerRequest(t *testing.T) {
	request := uosContainerRequest("itest-network", "example/uos:test")
	if request.Image != "example/uos:test" {
		t.Fatalf("image = %q", request.Image)
	}
	if !reflect.DeepEqual(request.ExposedPorts, []string{"443/tcp"}) {
		t.Fatalf("exposed ports = %#v", request.ExposedPorts)
	}
	if request.WaitingFor == nil {
		t.Fatal("UOS request has no health wait strategy")
	}
	if request.HostConfigModifier == nil {
		t.Fatal("UOS request has no host config modifier")
	}
}

func TestEmulatorContainerRequestUsesImageOverride(t *testing.T) {
	request := emulatorContainerRequest(
		"itest-network",
		itestImages{emulator: "example/emu:test"},
		"http://172.18.0.2:8080/inform",
		[]emu.DeviceSpec{{
			MAC: "00:27:22:e0:00:01", Model: "UGW3", IP: "192.168.1.242",
		}},
	)
	if request.Image != "example/emu:test" {
		t.Fatalf("image = %q", request.Image)
	}
	if request.Context != "" {
		t.Fatalf("build context = %q, want none", request.Context)
	}
	wantCommand := []string{
		"-inform", "http://172.18.0.2:8080/inform",
		"-mac", "00:27:22:e0:00:01",
		"-model", "UGW3",
		"-ip", "192.168.1.242",
	}
	if !reflect.DeepEqual(request.Cmd, wantCommand) {
		t.Fatalf("command = %#v, want %#v", request.Cmd, wantCommand)
	}
}

func TestEmulatorContainerRequestBuildsCheckoutByDefault(t *testing.T) {
	request := emulatorContainerRequest(
		"itest-network",
		itestImages{},
		"http://172.18.0.2:8080/inform",
		[]emu.DeviceSpec{
			{MAC: "00:27:22:e0:00:01", Model: "UGW3", IP: "192.168.1.242"},
			{MAC: "00:27:22:e0:00:11", Model: "USWED74", IP: "192.168.1.243"},
		},
	)
	if request.Image != "" {
		t.Fatalf("image = %q, want source build", request.Image)
	}
	if request.Context != "." || request.Dockerfile != "Dockerfile" {
		t.Fatalf("source build = context %q Dockerfile %q", request.Context, request.Dockerfile)
	}
	if request.Env["SIM_DEVICES"] == "" {
		t.Fatal("fleet request has no SIM_DEVICES payload")
	}
}
