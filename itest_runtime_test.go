//go:build integration

package emu_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/internal/adopt"
	"github.com/jamesbraid/unifi-emu/testkit"
)

type itestHarness struct {
	t             *testing.T
	ctx           context.Context
	cancel        context.CancelFunc
	th            *testkit.Harness
	evidence      string
	apiURL        string
	apiPort       string
	controllerIP  string
	pending       []adopt.Device
	final         []adopt.Device
	emulatorImage string
}

func (h *itestHarness) images() itestImages {
	images := loadITestImages()
	if h.emulatorImage != "" {
		images.emulator = h.emulatorImage
	}
	return images
}

func startClassicHarness(t *testing.T) *itestHarness {
	t.Helper()
	images := loadITestImages()
	emulatorImage := os.Getenv("UNIFI_EMU_ITEST_EMULATOR_IMAGE")
	if emulatorImage == "" && images.emulator != "" {
		emulatorImage = images.emulator
	}

	cfg := testkit.HarnessConfig{
		TestName:        t.Name(),
		Timeout:         12 * time.Minute,
		ControllerImage: images.classic,
		ControllerPort:  "8443",
		EmulatorImage:   emulatorImage,
		CatalogPath:     os.Getenv("UNIFI_EMU_RUNTIME_CATALOG"),
		MacBase:         itestMACBase,
		IpBase:          itestIPBase,
		IsUOS:           false,
	}

	th, err := testkit.NewHarness(cfg)
	if err != nil {
		t.Fatalf("start classic harness: %v", err)
	}

	h := &itestHarness{
		t:             t,
		ctx:           th.Ctx,
		cancel:        th.Cancel,
		th:            th,
		evidence:      th.Evidence,
		apiURL:        th.ApiURL,
		apiPort:       th.ApiPort,
		controllerIP:  th.ControllerIP,
		emulatorImage: emulatorImage,
	}
	t.Cleanup(h.close)
	return h
}

func startUOSHarness(t *testing.T) *itestHarness {
	t.Helper()
	images := loadITestImages()
	emulatorImage := os.Getenv("UNIFI_EMU_ITEST_EMULATOR_IMAGE")
	if emulatorImage == "" && images.emulator != "" {
		emulatorImage = images.emulator
	}

	cfg := testkit.HarnessConfig{
		TestName:        t.Name(),
		Timeout:         15 * time.Minute,
		ControllerImage: images.uos,
		ControllerPort:  "443",
		EmulatorImage:   emulatorImage,
		CatalogPath:     os.Getenv("UNIFI_EMU_RUNTIME_CATALOG"),
		MacBase:         itestMACBase,
		IpBase:          itestIPBase,
		IsUOS:           true,
	}

	th, err := testkit.NewHarness(cfg)
	if err != nil {
		t.Fatalf("start UOS harness: %v", err)
	}

	h := &itestHarness{
		t:             t,
		ctx:           th.Ctx,
		cancel:        th.Cancel,
		th:            th,
		evidence:      th.Evidence,
		apiURL:        th.ApiURL,
		apiPort:       th.ApiPort,
		controllerIP:  th.ControllerIP,
		emulatorImage: emulatorImage,
	}
	t.Cleanup(h.close)
	h.waitUOSReady()
	return h
}

func (h *itestHarness) startEmulator(specs []emu.DeviceSpec) {
	h.t.Helper()
	h.th.Config.EmulatorImage = h.emulatorImage
	if err := h.th.StartRuntimes(specs); err != nil {
		h.t.Fatalf("start emulator: %v", err)
	}
}

func (h *itestHarness) startEmulatorAdopting(specs []emu.DeviceSpec) {
	h.t.Helper()
	h.th.Config.EmulatorImage = h.emulatorImage
	adoptURL := "https://" + h.controllerIP + ":" + h.apiPort
	h.th.Config.ExtraEnv = map[string]string{
		"SIM_ADOPT":          "1",
		"SIM_ADOPT_URL":      adoptURL,
		"SIM_ADOPT_USERNAME": itestControllerUser,
		"SIM_ADOPT_PASSWORD": itestControllerPassword,
	}
	if err := h.th.StartRuntimes(specs); err != nil {
		h.t.Fatalf("start emulator adopting: %v", err)
	}
}

func (h *itestHarness) startEmulatorModels(modelsCSV string) {
	h.t.Helper()
	h.th.Config.EmulatorImage = h.emulatorImage
	h.th.Config.ExtraEnv = map[string]string{
		"SIM_MODELS":   modelsCSV,
		"SIM_MAC_BASE": itestMACBase,
		"SIM_IP_BASE":  itestIPBase,
	}
	specs := parseModelsCSV(modelsCSV)
	if err := h.th.StartRuntimes(specs); err != nil {
		h.t.Fatalf("start emulator models: %v", err)
	}
}

func (h *itestHarness) recordPending(device adopt.Device) {
	h.pending = append(h.pending, device)
	if err := writeJSON(filepath.Join(h.evidence, "pending-devices.json"), h.pending); err != nil {
		h.t.Errorf("write pending device evidence: %v", err)
	}
}

func (h *itestHarness) recordListing(client adopter, site string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, err := client.Devices(ctx, site)
	if err != nil {
		return fmt.Sprintf("controller listing unavailable: %v", err)
	}
	if err := writeJSON(filepath.Join(h.evidence, "devices-at-timeout.json"), list); err != nil {
		h.t.Errorf("write device listing evidence: %v", err)
	}
	if len(list) == 0 {
		return "controller listed no devices at all"
	}
	summary := make([]string, 0, len(list))
	for _, d := range list {
		summary = append(summary, fmt.Sprintf("%s/%s state=%d", d.MAC, d.Model, d.State))
	}
	return fmt.Sprintf("controller listed %d devices: %s", len(list), strings.Join(summary, ", "))
}

func (h *itestHarness) recordFinal(device adopt.Device) {
	h.final = append(h.final, device)
	if err := writeJSON(filepath.Join(h.evidence, "final-devices.json"), h.final); err != nil {
		h.t.Errorf("write final device evidence: %v", err)
	}
}

func (h *itestHarness) requireEmulatorRunning() {
	h.t.Helper()
	if err := h.th.RequireRuntimesRunning(); err != nil {
		h.t.Fatal(err)
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
	return h.th.ExecReady(h.ctx, phase, command)
}

func (h *itestHarness) close() {
	writeJSON(filepath.Join(h.evidence, "pending-devices.json"), h.pending)
	writeJSON(filepath.Join(h.evidence, "final-devices.json"), h.final)
	if h.th != nil {
		h.th.Close()
	}
}

func configureContainerRuntime(t *testing.T) {
	t.Helper()
	if err := testkit.ConfigureContainerRuntime(); err != nil {
		t.Fatalf("configure container runtime: %v", err)
	}
}
