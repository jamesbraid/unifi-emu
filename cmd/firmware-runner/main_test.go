package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/internal/firmwarerunner"
)

func TestRunDryRunPrintsPlanWithoutCallingDocker(t *testing.T) {
	dir := t.TempDir()
	tarData := cliRootFSTar(t)
	tarPath := filepath.Join(dir, "rootfs.tar")
	metaPath := filepath.Join(dir, "rootfs.json")
	if err := os.WriteFile(tarPath, tarData, 0o644); err != nil {
		t.Fatal(err)
	}
	writeCLIJSON(t, metaPath, map[string]any{
		"source_firmware_digest": "62c941a6d3a63afc851ee3b4a69ba33b36d57ae78730e13fac54223383422c22",
		"platform":               "UXGENT",
		"version":                "5.0.16.30689",
		"nested_artifact_path":   "firmware.bin/rootfs",
		"tar_digest":             fmt.Sprintf("%x", sha256.Sum256(tarData)),
		"entry_count":            3,
	})
	profilePath := filepath.Join(dir, "profile.json")
	if err := os.Mkdir(filepath.Join(dir, "overlay"), 0o755); err != nil {
		t.Fatal(err)
	}
	agentDigest := fmt.Sprintf(
		"%x", sha256.Sum256(cliAArch64ELF("/lib/ld-linux-aarch64.so.1")),
	)
	writeCLIJSON(t, profilePath, map[string]any{
		"name":                  "uxgent",
		"platform":              "UXGENT",
		"firmware_digest":       "62c941a6d3a63afc851ee3b4a69ba33b36d57ae78730e13fac54223383422c22",
		"firmware_version":      "5.0.16.30689",
		"agent_digest":          agentDigest,
		"architecture":          "aarch64",
		"interpreter":           "/lib/ld-linux-aarch64.so.1",
		"runner_image":          "runner:test",
		"container_name":        "firmware-uxgent",
		"startup_grace_seconds": 1,
		"overlay_dir":           "overlay",
		"command":               []string{"chroot", "/firmware", "/usr/bin/mcad"},
	})

	var output bytes.Buffer
	called := false
	err := run(context.Background(), []string{
		"-profile", profilePath,
		"-rootfs-json", metaPath,
		"-rootfs-tar", tarPath,
		"-work-dir", filepath.Join(dir, "work"),
		"-network", "firmware-net",
		"-inform-url", "http://controller:8080/inform",
		"-dry-run",
	}, &output, func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("dry-run invoked Docker")
	}
	var plan firmwarerunner.Plan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatalf("dry-run output is not a launch plan: %v\n%s", err, output.Bytes())
	}
	if plan.Profile != "uxgent" || plan.Network != "firmware-net" ||
		plan.Version != "5.0.16.30689" {
		t.Fatalf("dry-run plan = %+v", plan)
	}
}

func TestRunRequiresExtractorPairAndNetwork(t *testing.T) {
	for name, args := range map[string][]string{
		"rootfs json": {"-profile", "profile.json", "-rootfs-tar", "rootfs.tar", "-work-dir", "work", "-network", "net"},
		"rootfs tar":  {"-profile", "profile.json", "-rootfs-json", "rootfs.json", "-work-dir", "work", "-network", "net"},
		"network":     {"-profile", "profile.json", "-rootfs-json", "rootfs.json", "-rootfs-tar", "rootfs.tar", "-work-dir", "work"},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(context.Background(), args, &bytes.Buffer{}, nil)
			if err == nil || !strings.Contains(err.Error(), "required") {
				t.Fatalf("required flag error = %v", err)
			}
		})
	}
}

func TestRunHelpWritesUsage(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"-h"}, &output, nil); err != nil {
		t.Fatalf("help returned error: %v", err)
	}
	if !strings.Contains(output.String(), "Usage of firmware-runner:") ||
		!strings.Contains(output.String(), "-rootfs-json") {
		t.Fatalf("help output = %q", output.String())
	}
}

func TestExecutePlanRunsContainerThenPostStartCommand(t *testing.T) {
	plan := firmwarerunner.Plan{
		ContainerName:       "unifi-firmware-uxgent",
		RunnerImage:         "runner:test",
		Network:             "firmware-net",
		RootFS:              "/tmp/rootfs",
		StartupGraceSeconds: 2,
		Command:             []string{"chroot", "/firmware", "/usr/bin/mcad"},
		PostStart: &firmwarerunner.PostStart{
			Socket: "/firmware/tmp/.mcad", TimeoutSeconds: 2,
			Command: []string{"chroot", "/firmware", "/usr/bin/mca-cli-op", "set-inform", "http://controller:8080/inform"},
		},
	}
	var calls [][]string
	exec := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		switch len(calls) {
		case 1:
			return []byte("container-id\n"), nil
		case 2:
			return []byte(`{"Running":true,"Status":"running","ExitCode":0}`), nil
		}
		return nil, nil
	}
	if err := executePlanWithWait(context.Background(), plan, exec, noWait); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		append([]string{"docker"}, plan.DockerArgs()...),
		{"docker", "inspect", "--format", "{{json .State}}", "unifi-firmware-uxgent"},
		{"docker", "exec", "unifi-firmware-uxgent", "test", "-S", "/firmware/tmp/.mcad"},
		{"docker", "exec", "unifi-firmware-uxgent", "chroot", "/firmware", "/usr/bin/mca-cli-op", "set-inform", "http://controller:8080/inform"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("command calls = %#v\nwant %#v", calls, want)
	}
}

func TestExecutePlanRejectsContainerThatExitsDuringStartup(t *testing.T) {
	plan := firmwarerunner.Plan{
		ContainerName:       "unifi-firmware-u7pro",
		RunnerImage:         "runner:test",
		Network:             "firmware-net",
		RootFS:              "/tmp/rootfs",
		StartupGraceSeconds: 3,
		Command:             []string{"mcad"},
	}
	var waited time.Duration
	var calls [][]string
	command := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		switch len(calls) {
		case 1:
			return []byte("container-id"), nil
		case 2:
			return []byte(`{"Running":false,"Status":"exited","ExitCode":127,"Error":""}`), nil
		case 3:
			return []byte("chroot: /usr/bin/mcad: not found"), nil
		case 4:
			return []byte("unifi-firmware-u7pro"), nil
		default:
			t.Fatalf("unexpected command: %s %v", name, args)
			return nil, nil
		}
	}
	wait := func(_ context.Context, duration time.Duration) error {
		waited = duration
		return nil
	}
	err := executePlanWithWait(context.Background(), plan, command, wait)
	if err == nil ||
		!strings.Contains(err.Error(), `"Status":"exited"`) ||
		!strings.Contains(err.Error(), "chroot: /usr/bin/mcad: not found") {
		t.Fatalf("startup failure = %v", err)
	}
	if waited != 3*time.Second {
		t.Fatalf("startup grace = %s, want 3s", waited)
	}
	wantTail := [][]string{
		{"docker", "logs", "--tail", "100", "unifi-firmware-u7pro"},
		{"docker", "rm", "-f", "unifi-firmware-u7pro"},
	}
	if !reflect.DeepEqual(calls[len(calls)-2:], wantTail) {
		t.Fatalf("failure diagnostics/cleanup = %#v, want %#v", calls, wantTail)
	}
}

func TestExecutePlanReportsDockerFailureOutput(t *testing.T) {
	plan := firmwarerunner.Plan{
		ContainerName: "firmware", RunnerImage: "runner:test",
		Network: "net", RootFS: "/tmp/root", Command: []string{"mcad"},
	}
	err := executePlan(context.Background(), plan,
		func(context.Context, string, ...string) ([]byte, error) {
			return []byte("network net not found"), errors.New("exit status 1")
		})
	if err == nil || !strings.Contains(err.Error(), "network net not found") {
		t.Fatalf("Docker failure = %v", err)
	}
}

func TestExecutePlanRemovesContainerAfterPostStartFailure(t *testing.T) {
	plan := firmwarerunner.Plan{
		ContainerName: "firmware", RunnerImage: "runner:test",
		Network: "net", RootFS: "/tmp/root", Command: []string{"mcad"},
		PostStart: &firmwarerunner.PostStart{
			Socket: "/firmware/tmp/.mcad", TimeoutSeconds: 2,
			Command: []string{"set-inform"},
		},
	}
	var calls [][]string
	command := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		switch len(calls) {
		case 1:
			return nil, nil
		case 2:
			return []byte(`{"Running":true,"Status":"running","ExitCode":0}`), nil
		case 3:
			return nil, nil
		case 4:
			return []byte("set-inform failed"), errors.New("exit status 1")
		case 5:
			return []byte("firmware"), nil
		default:
			t.Fatalf("unexpected command: %s %v", name, args)
			return nil, nil
		}
	}
	err := executePlanWithWait(context.Background(), plan, command, noWait)
	if err == nil || !strings.Contains(err.Error(), "set-inform failed") {
		t.Fatalf("post-start failure = %v", err)
	}
	wantCleanup := []string{"docker", "rm", "-f", "firmware"}
	if !reflect.DeepEqual(calls[len(calls)-1], wantCleanup) {
		t.Fatalf("cleanup call = %#v, want %#v", calls[len(calls)-1], wantCleanup)
	}
}

func noWait(context.Context, time.Duration) error {
	return nil
}

func cliRootFSTar(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, name := range []string{"usr/", "usr/bin/"} {
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeDir, Mode: 0o755,
		}); err != nil {
			t.Fatal(err)
		}
	}
	agent := cliAArch64ELF("/lib/ld-linux-aarch64.so.1")
	if err := writer.WriteHeader(&tar.Header{
		Name: "usr/bin/mcad", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(agent)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(agent); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func cliAArch64ELF(interpreter string) []byte {
	interp := append([]byte(interpreter), 0)
	data := make([]byte, 64+56+len(interp))
	copy(data[:16], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 3)
	binary.LittleEndian.PutUint16(data[18:], 183)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint64(data[32:], 64)
	binary.LittleEndian.PutUint16(data[52:], 64)
	binary.LittleEndian.PutUint16(data[54:], 56)
	binary.LittleEndian.PutUint16(data[56:], 1)
	binary.LittleEndian.PutUint32(data[64:], 3)
	binary.LittleEndian.PutUint64(data[72:], 120)
	binary.LittleEndian.PutUint64(data[96:], uint64(len(interp)))
	copy(data[120:], interp)
	return data
}

func writeCLIJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}
