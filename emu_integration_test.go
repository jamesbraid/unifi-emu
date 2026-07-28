//go:build integration

package emu_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/internal/adopt"
	"github.com/testcontainers/testcontainers-go"
)

func TestClassicUGWLive(t *testing.T) {
	h := startClassicHarness(t)
	spec := emu.DeviceSpec{
		MAC:   "00:27:22:e0:00:01",
		Model: "UGW3",
		IP:    "192.168.1.242",
	}
	h.startEmulator([]emu.DeviceSpec{spec})

	client := adopt.New(h.apiURL, adopt.Classic)
	if err := client.Login(h.ctx, "admin", "admin"); err != nil {
		t.Fatalf("login: %v", err)
	}
	adoptAndWaitConnected(t, h.ctx, h, client, spec.Model, spec.MAC)
}

// TestClassicUXGLive adopts a next-gen gateway (UXGENT / "Gateway Enterprise",
// type uxg) to CONNECTED against the classic Network application. The emu runs
// as a peer container on the shared network, so the post-adopt inform_ip the
// controller hands back round-trips container-to-container -- the handshake a
// host-loopback harness cannot complete on docker-in-a-VM. Type is left blank
// so it derives from the profile (uxg), proving the profile drives the full
// inform/adopt/provision flow, not just the initial pending document.
func TestClassicUXGLive(t *testing.T) {
	h := startClassicHarness(t)
	spec := emu.DeviceSpec{
		MAC:   "00:27:22:e0:00:02",
		Model: "UXGENT",
		IP:    "192.168.1.248",
	}
	h.startEmulator([]emu.DeviceSpec{spec})

	client := adopt.New(h.apiURL, adopt.Classic)
	if err := client.Login(h.ctx, "admin", "admin"); err != nil {
		t.Fatalf("login: %v", err)
	}
	adoptAndWaitConnected(t, h.ctx, h, client, spec.Model, spec.MAC)
}

// fleetSpecs is the live fleet: exactly one gateway (the controller allows
// one per site), two switches, and two access points. The reported IPs are
// distinct but arbitrary because the controller never routes to them.
var fleetSpecs = []emu.DeviceSpec{
	{MAC: "00:27:22:e0:00:01", Model: "UGW3", IP: "192.168.1.242"},
	{MAC: "00:27:22:e0:00:11", Model: "USWED74", IP: "192.168.1.243"},
	{MAC: "00:27:22:e0:00:12", Model: "USM8P", IP: "192.168.1.244"},
	{MAC: "00:27:22:e0:00:21", Model: "U7MP", IP: "192.168.1.245"},
	{MAC: "00:27:22:e0:00:22", Model: "U7PRO", IP: "192.168.1.246"},
}

func TestClassicFleetLive(t *testing.T) {
	h := startClassicHarness(t)
	h.startEmulator(fleetSpecs)

	client := adopt.New(h.apiURL, adopt.Classic)
	if err := client.Login(h.ctx, "admin", "admin"); err != nil {
		t.Fatalf("login: %v", err)
	}

	// This controller build rejects bursts and documents that are only
	// seconds old, so adopt one device fully before moving to the next.
	for _, spec := range fleetSpecs {
		adoptAndWaitConnected(t, h.ctx, h, client, spec.Model, spec.MAC)
	}
}

// TestClassicRandomFleetLive boots a reproducible pseudo-random selection of
// models via SIM_MODELS (exercising the CLI selector and MAC/IP expansion end
// to end) and adopts each to CONNECTED. The seed is logged and honored from
// SIM_ITEST_SEED so a CI failure reproduces exactly.
func TestClassicRandomFleetLive(t *testing.T) {
	seed := seedFromEnv()
	typeOf := func(m string) (string, bool) {
		p, ok := emu.Profile(m)
		return p.Type, ok
	}
	models := selectFleet(adoptableModels, typeOf, seed)
	t.Logf("random fleet seed=%d models=%v (reproduce with SIM_ITEST_SEED=%d)", seed, models, seed)

	macs, err := macsForModels(itestMACBase, len(models))
	if err != nil {
		t.Fatalf("compute MACs for %v: %v", models, err)
	}

	h := startClassicHarness(t)
	h.startEmulatorModels(strings.Join(models, ","))

	client := adopt.New(h.apiURL, adopt.Classic)
	if err := client.Login(h.ctx, "admin", "admin"); err != nil {
		t.Fatalf("login: %v", err)
	}

	// One device at a time: this controller rejects bursts.
	for i, mac := range macs {
		t.Logf("adopting %s (mac %s)", models[i], mac)
		device := adoptAndWaitConnected(t, h.ctx, h, client, models[i], mac)
		t.Logf("adopted mac %s: controller model=%s state=%d", mac, device.Model, device.State)
	}
}

func TestUOSAPUpgradeLive(t *testing.T) {
	h := startUOSHarness(t)
	spec := emu.DeviceSpec{
		MAC:   "00:27:22:e0:00:31",
		Model: "U7PRO",
		IP:    "192.168.1.247",
	}
	h.startEmulator([]emu.DeviceSpec{spec})

	client := adopt.New(h.apiURL, adopt.UniFiOS)
	if err := client.Login(h.ctx, "admin", "admin"); err != nil {
		t.Fatalf("login: %v", err)
	}

	device := adoptAndWaitConnected(t, h.ctx, h, client, spec.Model, spec.MAC)
	if device.Version != "8.6.11.18870" {
		t.Fatalf("%s controller firmware = %q, want upgraded 8.6.11.18870",
			spec.MAC, device.Version)
	}
	t.Logf("%s UOS upgrade complete: state=%d adopted=%v version=%s",
		spec.MAC, device.State, device.Adopted, device.Version)
}

// adopter is the controller-side surface shared by the classic Network
// application and UniFi OS clients.
type adopter interface {
	Adopt(ctx context.Context, site, mac string) error
	DeviceByMAC(ctx context.Context, site, mac string) (adopt.Device, error)
	Devices(ctx context.Context, site string) ([]adopt.Device, error)
	WaitAdopted(ctx context.Context, site, mac string) (adopt.Device, error)
}

// The credentials every controller image under test ships with.
const (
	itestControllerUser     = "admin"
	itestControllerPassword = "admin"
)

// liveAdoptTimeout budgets one device's whole trip from informing to
// connected when the container is the one adopting it.
const liveAdoptTimeout = 6 * time.Minute

// TestClassicContainerAdoptLive is the proof that the container adopts its
// own devices: the emulator runs with SIM_ADOPT and this test never issues
// an adopt command, only watching the controller until the fleet reports
// connected. Two devices, so the sequential fleet loop is exercised rather
// than a single lucky adopt.
func TestClassicContainerAdoptLive(t *testing.T) {
	h := startClassicHarness(t)
	specs := []emu.DeviceSpec{
		{MAC: "00:27:22:e0:00:41", Model: "U7PRO", IP: "192.168.1.181"},
		{MAC: "00:27:22:e0:00:42", Model: "USM8P", IP: "192.168.1.182"},
	}
	h.startEmulatorAdopting(specs)

	client := adopt.New(h.apiURL, adopt.Classic)
	if err := client.Login(h.ctx, itestControllerUser, itestControllerPassword); err != nil {
		t.Fatalf("login: %v", err)
	}
	for _, spec := range specs {
		device := waitContainerAdopted(t, h, client, spec.MAC)
		h.recordFinal(device)
		t.Logf("%s adopted by the container: state=%d adopted=%v model=%s ip=%s",
			device.MAC, device.State, device.Adopted, device.Model, device.IP)
	}
}

// waitContainerAdopted watches the controller until mac reports connected,
// checking on every poll that the emulator is still alive. That check is the
// point: a container whose adoption fails exits non-zero, so its status
// carries the real diagnosis and there is no reason to wait out the clock.
func waitContainerAdopted(t *testing.T, h *itestHarness, client *adopt.Client, mac string) adopt.Device {
	t.Helper()
	ctx, stop := context.WithTimeout(h.ctx, liveAdoptTimeout)
	defer stop()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var last adopt.Device
	var lastErr error
	for {
		device, err := client.DeviceByMAC(ctx, "default", mac)
		if err == nil {
			last, lastErr = device, nil
			if device.State == 1 && device.Adopted {
				return device
			}
		} else {
			lastErr = err
		}
		h.requireEmulatorRunning()
		select {
		case <-ctx.Done():
			t.Fatalf("%s never adopted by the container: %v (last state %d adopted=%v, last error %v)",
				mac, ctx.Err(), last.State, last.Adopted, lastErr)
		case <-tick.C:
		}
	}
}

// adoptAndWaitConnected drives one device through the complete live flow:
// wait for the pending document, adopt with the controller's known
// too-young-document retry, then require controller state 1/adopted and a
// still-running emulator container.
//
// model names the device in every message. A MAC alone is ambiguous -- the
// same address is the lone device of a single-model test and index 1 of any
// fleet -- and the model is the first thing to suspect when one device out of
// a fleet never adopts.
func adoptAndWaitConnected(
	t *testing.T,
	ctx context.Context,
	h *itestHarness,
	client adopter,
	model string,
	mac string,
) adopt.Device {
	t.Helper()
	device, err := adoptAndWait(t, ctx, h, client, model, mac)
	if err != nil {
		t.Fatal(err)
	}
	return device
}

// adoptAndWait is adoptAndWaitConnected's body, reporting failure as an error
// instead of ending the test. The catalog sweep drives many models through one
// controller and has to record a model that will not adopt and carry on to the
// next, which a t.Fatalf cannot do.
func adoptAndWait(
	t *testing.T,
	ctx context.Context,
	h *itestHarness,
	client adopter,
	model string,
	mac string,
) (adopt.Device, error) {
	t.Helper()
	const site = "default"
	dev := fmt.Sprintf("%s %s", model, mac)

	pendingCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	var last adopt.Device
	seen := false
	for {
		device, err := client.DeviceByMAC(pendingCtx, site, mac)
		if err == nil {
			last, seen = device, true
			if device.State == 1 && device.Adopted {
				return last, fmt.Errorf("%s already adopted on a fresh controller", dev)
			}
			if device.State == 2 {
				h.recordPending(device)
				break
			}
		}
		h.requireEmulatorRunning()
		select {
		case <-pendingCtx.Done():
			if seen {
				return last, fmt.Errorf("%s never appeared pending: %v (last state %d adopted=%v)",
					dev, pendingCtx.Err(), last.State, last.Adopted)
			}
			return last, fmt.Errorf("%s never appeared pending: %v (never listed, last error %v; %s)",
				dev, pendingCtx.Err(), err, h.recordListing(client, site))
		case <-time.After(2 * time.Second):
		}
	}

	adoptCtx, stop := context.WithTimeout(ctx, 3*time.Minute)
	defer stop()
	for {
		err := client.Adopt(adoptCtx, site, mac)
		if err == nil {
			break
		}
		if !strings.Contains(strings.ToLower(err.Error()), "cannotadopt") {
			return adopt.Device{}, fmt.Errorf("adopt %s: %w", dev, err)
		}
		if device, lookupErr := client.DeviceByMAC(adoptCtx, site, mac); lookupErr == nil && device.Adopted {
			t.Logf("%s adopt returned CannotAdopt but the document is adopted; continuing", dev)
			break
		}
		t.Logf("%s adopt rejected (%v); retrying", dev, err)
		h.requireEmulatorRunning()
		select {
		case <-adoptCtx.Done():
			return adopt.Device{}, fmt.Errorf("%s never adopted: %v (last adopt error %v)", dev, adoptCtx.Err(), err)
		case <-time.After(10 * time.Second):
		}
	}

	waitCtx, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	device, err := client.WaitAdopted(waitCtx, site, mac)
	if err != nil {
		return device, fmt.Errorf("%s controller-side adoption: %w", dev, err)
	}
	h.requireEmulatorRunning()
	h.recordFinal(device)
	t.Logf("%s controller: state=%d adopted=%v model=%s ip=%s name=%s",
		dev, device.State, device.Adopted, device.Model, device.IP, device.Name)
	return device, nil
}

func TestClassicOCIFixtureLive(t *testing.T) {
	images := loadITestImages()
	emulatorImage := os.Getenv("UNIFI_EMU_ITEST_EMULATOR_IMAGE")
	if emulatorImage == "" {
		emulatorImage = images.emulator
	}

	fixtureImage := "public-fixture:local"
	if emulatorImage != "" {
		fixtureImage = emulatorImage
	} else {
		t.Log("Building local emulator image via Testcontainers dummy container")
		ctx := context.Background()
		dummy, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				FromDockerfile: testcontainers.FromDockerfile{
					Context:    ".",
					Dockerfile: "Dockerfile",
					Repo:       "public-fixture",
					Tag:        "local",
					BuildArgs: map[string]*string{
						"BUILDPLATFORM": pointerTo("linux/" + runtime.GOARCH),
						"TARGETOS":      pointerTo("linux"),
						"TARGETARCH":    pointerTo(runtime.GOARCH),
					},
				},
			},
			Started: false,
		})
		if err != nil {
			t.Fatalf("failed to build local emulator image: %v", err)
		}
		t.Cleanup(func() {
			dummy.Terminate(ctx)
		})
	}

	catalogContent := fmt.Sprintf(`{
		"version": 1,
		"runtimes": [
			{
				"name": "fixture",
				"models": ["U7PRO"],
				"image": %q,
				"isolation": "device",
				"cap_add": ["NET_ADMIN"]
			}
		]
	}`, fixtureImage)

	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalogPath, []byte(catalogContent), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UNIFI_EMU_RUNTIME_CATALOG", catalogPath)

	h := startClassicHarness(t)
	spec := emu.DeviceSpec{
		MAC:   "00:27:22:e0:00:02",
		Model: "U7PRO",
		IP:    "192.168.1.243",
	}
	h.startEmulator([]emu.DeviceSpec{spec})

	client := adopt.New(h.apiURL, adopt.Classic)
	if err := client.Login(h.ctx, "admin", "admin"); err != nil {
		t.Fatalf("login: %v", err)
	}
	adoptAndWaitConnected(t, h.ctx, h, client, spec.Model, spec.MAC)
}

func pointerTo(s string) *string {
	return &s
}
