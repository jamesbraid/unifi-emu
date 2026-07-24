package emu_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	emu "github.com/jamesbraid/unifi-emu"
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
	devices := []emu.Device{{
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
