//go:build integration

package emu_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
)

type itestHarness struct {
	t            *testing.T
	ctx          context.Context
	cancel       context.CancelFunc
	network      *testcontainers.DockerNetwork
	controller   testcontainers.Container
	emulator     testcontainers.Container
	evidence     string
	apiURL       string
	controllerIP string
	pending      []emu.Device
	final        []emu.Device
}

func startClassicHarness(t *testing.T) *itestHarness {
	t.Helper()
	h := newITestHarness(t, 12*time.Minute)
	h.startController(classicContainerRequest(h.network.Name, loadITestImages().classic), "8443/tcp")
	return h
}

func startUOSHarness(t *testing.T) *itestHarness {
	t.Helper()
	h := newITestHarness(t, 15*time.Minute)
	h.startController(uosContainerRequest(h.network.Name, loadITestImages().uos), "443/tcp")
	h.waitUOSReady()
	return h
}

func newITestHarness(t *testing.T, timeout time.Duration) *itestHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	evidence := evidenceDir(t.Name())
	if err := os.RemoveAll(evidence); err != nil {
		cancel()
		t.Fatalf("clear evidence directory %s: %v", evidence, err)
	}
	if err := os.MkdirAll(evidence, 0o755); err != nil {
		cancel()
		t.Fatalf("create evidence directory %s: %v", evidence, err)
	}
	h := &itestHarness{
		t:        t,
		ctx:      ctx,
		cancel:   cancel,
		evidence: evidence,
	}
	network, err := tcnetwork.New(ctx)
	h.network = network
	t.Cleanup(h.close)
	if err != nil {
		t.Fatalf("create Testcontainers network: %v", err)
	}
	return h
}

func (h *itestHarness) startController(request testcontainers.ContainerRequest, apiPort string) {
	h.t.Helper()
	controller, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: request,
		Started:          true,
	})
	h.controller = controller
	if err != nil {
		h.t.Fatalf("start controller container: %v", err)
	}
	h.apiURL = h.mappedHTTPSURL(apiPort)
	h.controllerIP, err = controller.ContainerIP(h.ctx)
	if err != nil {
		h.t.Fatalf("resolve controller container IPv4: %v", err)
	}
	if h.controllerIP == "" {
		h.t.Fatal("resolve controller container IPv4: Testcontainers returned an empty address")
	}
}

func (h *itestHarness) mappedHTTPSURL(port string) string {
	h.t.Helper()
	host, err := h.controller.Host(h.ctx)
	if err != nil {
		h.t.Fatalf("resolve controller API host: %v", err)
	}
	mapped, err := h.controller.MappedPort(h.ctx, port)
	if err != nil {
		h.t.Fatalf("resolve controller API port %s: %v", port, err)
	}
	return "https://" + host + ":" + mapped.Port()
}

func (h *itestHarness) startEmulator(specs []emu.DeviceSpec) {
	h.t.Helper()
	informURL := "http://" + h.controllerIP + ":8080/inform"
	request := emulatorContainerRequest(h.network.Name, loadITestImages(), informURL, specs)
	emulator, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: request,
		Started:          true,
	})
	h.emulator = emulator
	if err != nil {
		h.t.Fatalf("start emulator container: %v", err)
	}
}

func (h *itestHarness) recordPending(device emu.Device) {
	h.pending = append(h.pending, device)
	if err := writeJSON(filepath.Join(h.evidence, "pending-devices.json"), h.pending); err != nil {
		h.t.Errorf("write pending device evidence: %v", err)
	}
}

func (h *itestHarness) recordFinal(device emu.Device) {
	h.final = append(h.final, device)
	if err := writeJSON(filepath.Join(h.evidence, "final-devices.json"), h.final); err != nil {
		h.t.Errorf("write final device evidence: %v", err)
	}
}

func (h *itestHarness) waitUOSReady() {
	h.t.Helper()
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var ownerErr, apiErr error
	for {
		ownerErr = h.execReady(
			"seeded owner",
			[]string{"sh", "-c", "test -s /unifi/logs/uos-seed-owner.log"},
		)
		apiErr = h.execReady(
			"UOS API",
			[]string{"curl", "-kfsS", "https://127.0.0.1/api/system"},
		)
		if ownerErr == nil && apiErr == nil {
			return
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatalf("wait for seeded-owner/API readiness: %v (owner: %v; API: %v)",
				h.ctx.Err(), ownerErr, apiErr)
		case <-deadline.C:
			h.t.Fatalf("wait for seeded-owner/API readiness: deadline exceeded (owner: %v; API: %v)",
				ownerErr, apiErr)
		case <-ticker.C:
		}
	}
}

func (h *itestHarness) execReady(phase string, command []string) error {
	code, output, err := h.controller.Exec(h.ctx, command)
	if err != nil {
		return fmt.Errorf("%s exec: %w", phase, err)
	}
	body, readErr := io.ReadAll(output)
	if readErr != nil {
		return fmt.Errorf("%s output: %w", phase, readErr)
	}
	if code != 0 {
		return fmt.Errorf("%s exit %d: %s", phase, code, body)
	}
	return nil
}

func (h *itestHarness) close() {
	captureCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	h.captureEvidence(captureCtx)
	if h.emulator != nil {
		if err := h.emulator.Terminate(captureCtx); err != nil {
			h.t.Errorf("terminate emulator container: %v", err)
		}
	}
	if h.controller != nil {
		if err := h.controller.Terminate(captureCtx); err != nil {
			h.t.Errorf("terminate controller container: %v", err)
		}
	}
	if h.network != nil {
		if err := h.network.Remove(captureCtx); err != nil {
			h.t.Errorf("remove Testcontainers network: %v", err)
		}
	}
	h.cancel()
}

func (h *itestHarness) captureEvidence(ctx context.Context) {
	if err := writeJSON(filepath.Join(h.evidence, "pending-devices.json"), h.pending); err != nil {
		h.writeEvidenceError("pending-devices.json", err)
	}
	if err := writeJSON(filepath.Join(h.evidence, "final-devices.json"), h.final); err != nil {
		h.writeEvidenceError("final-devices.json", err)
	}
	h.captureLogs(ctx, h.controller, "controller.log")
	h.captureLogs(ctx, h.emulator, "emulator.log")
	h.captureServerLog(ctx)
}

func (h *itestHarness) captureLogs(ctx context.Context, target testcontainers.Container, name string) {
	if target == nil {
		h.writeEvidenceError(name, errors.New("container was not started"))
		return
	}
	logs, err := target.Logs(ctx)
	if err != nil {
		h.writeEvidenceError(name, err)
		return
	}
	defer logs.Close()
	h.writeEvidenceReader(name, logs)
}

func (h *itestHarness) captureServerLog(ctx context.Context) {
	if h.controller == nil {
		h.writeEvidenceError("server.log", errors.New("controller container was not started"))
		return
	}
	var errs []error
	for _, source := range []string{
		"/usr/lib/unifi/logs/server.log",
		"/unifi/logs/server.log",
	} {
		reader, err := h.controller.CopyFileFromContainer(ctx, source)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", source, err))
			continue
		}
		h.writeEvidenceReader("server.log", reader)
		reader.Close()
		return
	}
	h.writeEvidenceError("server.log", errors.Join(errs...))
}

func (h *itestHarness) writeEvidenceReader(name string, reader io.Reader) {
	path := filepath.Join(h.evidence, name)
	file, err := os.Create(path)
	if err != nil {
		h.writeEvidenceError(name, err)
		return
	}
	if _, err := io.Copy(file, reader); err != nil {
		h.writeEvidenceError(name, err)
	}
	if err := file.Close(); err != nil {
		h.writeEvidenceError(name, err)
	}
}

func (h *itestHarness) writeEvidenceError(name string, err error) {
	if err == nil {
		return
	}
	path := filepath.Join(h.evidence, name+".error.txt")
	if writeErr := os.WriteFile(path, []byte(err.Error()+"\n"), 0o644); writeErr != nil {
		h.t.Errorf("write evidence error %s: %v", path, writeErr)
	}
}
