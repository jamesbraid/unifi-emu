//go:build integration

package emu_test

import (
	"context"
	"strings"
	"testing"
	"time"

	emu "github.com/jamesbraid/unifi-emu"
)

func TestClassicUGWLive(t *testing.T) {
	h := startClassicHarness(t)
	spec := emu.DeviceSpec{
		MAC:   "00:27:22:e0:00:01",
		Model: "UGW3",
		IP:    "192.168.1.242",
	}
	h.startEmulator([]emu.DeviceSpec{spec})

	client := emu.NewClassicClient(h.apiURL)
	if err := client.Login(h.ctx, "admin", "admin"); err != nil {
		t.Fatalf("login: %v", err)
	}
	adoptAndWaitConnected(t, h.ctx, h, client, spec.MAC)
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

	client := emu.NewClassicClient(h.apiURL)
	if err := client.Login(h.ctx, "admin", "admin"); err != nil {
		t.Fatalf("login: %v", err)
	}

	// This controller build rejects bursts and documents that are only
	// seconds old, so adopt one device fully before moving to the next.
	for _, spec := range fleetSpecs {
		adoptAndWaitConnected(t, h.ctx, h, client, spec.MAC)
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

	client := emu.NewUOSClient(h.apiURL)
	if err := client.Login(h.ctx, "admin", "admin"); err != nil {
		t.Fatalf("login: %v", err)
	}

	device := adoptAndWaitConnected(t, h.ctx, h, client, spec.MAC)
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
	DeviceByMAC(ctx context.Context, site, mac string) (emu.Device, error)
	WaitAdopted(ctx context.Context, site, mac string) (emu.Device, error)
}

// adoptAndWaitConnected drives one device through the complete live flow:
// wait for the pending document, adopt with the controller's known
// too-young-document retry, then require controller state 1/adopted and a
// still-running emulator container.
func adoptAndWaitConnected(
	t *testing.T,
	ctx context.Context,
	h *itestHarness,
	client adopter,
	mac string,
) emu.Device {
	t.Helper()
	const site = "default"

	pendingCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	var last emu.Device
	seen := false
	for {
		device, err := client.DeviceByMAC(pendingCtx, site, mac)
		if err == nil {
			last, seen = device, true
			if device.State == 1 && device.Adopted {
				t.Fatalf("%s already adopted on a fresh controller", mac)
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
				t.Fatalf("%s never appeared pending: %v (last state %d adopted=%v)",
					mac, pendingCtx.Err(), last.State, last.Adopted)
			}
			t.Fatalf("%s never appeared pending: %v (never listed, last error %v)",
				mac, pendingCtx.Err(), err)
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
			t.Fatalf("adopt %s: %v", mac, err)
		}
		if device, lookupErr := client.DeviceByMAC(adoptCtx, site, mac); lookupErr == nil && device.Adopted {
			t.Logf("%s adopt returned CannotAdopt but the document is adopted; continuing", mac)
			break
		}
		t.Logf("%s adopt rejected (%v); retrying", mac, err)
		h.requireEmulatorRunning()
		select {
		case <-adoptCtx.Done():
			t.Fatalf("%s never adopted: %v (last adopt error %v)", mac, adoptCtx.Err(), err)
		case <-time.After(10 * time.Second):
		}
	}

	waitCtx, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	device, err := client.WaitAdopted(waitCtx, site, mac)
	if err != nil {
		t.Fatalf("%s controller-side adoption: %v", mac, err)
	}
	h.requireEmulatorRunning()
	h.recordFinal(device)
	t.Logf("%s controller: state=%d adopted=%v model=%s ip=%s name=%s",
		mac, device.State, device.Adopted, device.Model, device.IP, device.Name)
	return device
}
