package firmwarerunner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUXGENTProfilePreparesProvenIdentityAndSetInform(t *testing.T) {
	profilePath := filepath.Join("..", "..", "firmware", "profiles", "uxgent.json")
	profile, err := LoadProfile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	plan, root := prepareCheckedProfile(t, profile, "arm64", "/lib/ld-linux-aarch64.so.1")

	systemInfo, err := os.ReadFile(filepath.Join(root, "proc/ubnthal/system.info"))
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "systemid=ea3e\ncpuid=00000001\nboardrevision=2\nserialno=00:27:22:e0:00:01\n"
	if !strings.HasPrefix(string(systemInfo), wantPrefix) {
		t.Fatalf("system.info does not preserve the forward-only parser order:\n%s", systemInfo)
	}
	if plan.ContainerMAC != "00:27:22:e0:00:01" {
		t.Fatalf("container MAC = %q", plan.ContainerMAC)
	}
	if plan.PostStart == nil ||
		!slices.Contains(plan.PostStart.Command, "http://controller:8080/inform") {
		t.Fatalf("post-start set-inform = %#v", plan.PostStart)
	}
	if _, err := os.Stat(filepath.Join(root, "opt/firmware-runner/udapi_stub.py")); err != nil {
		t.Fatalf("UDAPI stub was not materialized: %v", err)
	}
	tcp, err := os.ReadFile(filepath.Join(root, "proc/net/tcp"))
	if err != nil {
		t.Fatalf("synthetic sshd listener was not materialized: %v", err)
	}
	if !strings.Contains(string(tcp), "00000000:0016 00000000:0000 0A") {
		t.Fatalf("synthetic /proc/net/tcp does not report port 22 LISTEN:\n%s", tcp)
	}
}

func TestU7PROProfilePreparesQEMUBridgeAndBoardStatus(t *testing.T) {
	profilePath := filepath.Join("..", "..", "firmware", "profiles", "u7pro.json")
	profile, err := LoadProfile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	plan, root := prepareCheckedProfile(t, profile, "arm", "/lib/ld-musl-armhf.so.1")

	if !slices.Contains(plan.CapAdd, "NET_ADMIN") ||
		!slices.Contains(plan.CapAdd, "SYS_CHROOT") {
		t.Fatalf("U7PRO capabilities = %#v", plan.CapAdd)
	}
	command := strings.Join(plan.Command, " ")
	for _, want := range []string{
		"ip link add br0 type dummy",
		"ip link set br0 address 00:27:22:e0:00:22",
		"ip addr add 192.0.2.22/24 dev br0",
		"qemu-arm-static -B 0x100000000 /usr/bin/mcad",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("U7PRO command does not contain %q:\n%s", want, command)
		}
	}
	for path, want := range map[string]string{
		"proc/ubnthal/status/ControllerHost": "unifi\n",
		"proc/ubnthal/status/ControllerPort": "8080\n",
	} {
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q", path, body, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "var/run/ipready.br0")); err != nil {
		t.Errorf("ipready marker was not materialized: %v", err)
	}
	tcp, err := os.ReadFile(filepath.Join(root, "proc/net/tcp"))
	if err != nil || !strings.Contains(string(tcp), "00000000:0016 00000000:0000 0A") {
		t.Errorf("U7PRO synthetic sshd listener = %q, error %v", tcp, err)
	}
	meminfo, err := os.ReadFile(filepath.Join(root, "proc/meminfo"))
	if err != nil || !strings.Contains(string(meminfo), "MemTotal:        1048576 kB") {
		t.Errorf("U7PRO synthetic meminfo = %q, error %v", meminfo, err)
	}
}

func prepareCheckedProfile(
	t *testing.T,
	profile Profile,
	architecture, interpreter string,
) (Plan, string) {
	t.Helper()
	dir := t.TempDir()
	tarData := rootFSTar(t, architecture, interpreter)
	profile.AgentDigest = syntheticAgentDigest(t, architecture, interpreter)
	tarPath := filepath.Join(dir, "rootfs.tar")
	metaPath := filepath.Join(dir, "rootfs.json")
	writeFile(t, tarPath, tarData, 0o644)
	metadata := rootFSMetadataFixture(tarData, profile.Platform)
	metadata.SourceFirmwareDigest = profile.FirmwareDigest
	metadata.Version = profile.FirmwareVersion
	writeMetadata(t, metaPath, metadata)
	bundle, err := LoadBundle(metaPath, tarPath)
	if err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(dir, "work")
	plan, err := Prepare(Options{
		Bundle: bundle, Profile: profile, WorkDir: workDir,
		Network: "firmware-net", InformURL: "http://controller:8080/inform",
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan, filepath.Join(workDir, "rootfs")
}
