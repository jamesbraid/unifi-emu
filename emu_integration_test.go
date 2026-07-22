//go:build integration

package emu_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu"
)

// Live adoption proof against a real controller. Requires:
//
//	docker run -d --name unifi-itest-ctrl --network unifi-itest --ip 172.30.0.2 \
//	  -e SYSTEM_IP=127.0.0.1 -p 8443:8443 -p 8080:8080 ghcr.io/jamesbraid/unifi-network:sim
//
// (fresh — one gateway per site — and healthy), then:
//
//	UNIFI_EMU_TEST_INFORM_URL=http://127.0.0.1:8080/inform \
//	UNIFI_EMU_TEST_API_URL=https://localhost:8443 \
//	go test -tags integration -v .
//
// SYSTEM_IP=127.0.0.1 is what makes the post-adopt inform uri reachable
// from this host; scripts/itest.sh has the details. The fleet test
// adopts every device in fleetSpecs; the UGW test adopts one gateway.
//
// One live test per fresh controller: both tests adopt the same gateway
// MAC, so a single `go test -tags integration .` run makes the second
// test die on the fresh-fixture check. Recreate the controller between
// runs (scripts/itest.sh does this) and select one test with
// -run TestEmuAdoptsUGWLive or -run TestEmuAdoptsFleetLive.
func TestEmuAdoptsUGWLive(t *testing.T) {
	informURL, apiURL := liveEnv(t)
	mac := os.Getenv("UNIFI_EMU_TEST_MAC")
	if mac == "" {
		mac = "00:27:22:e0:00:01"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	e := emu.New(informURL)
	if err := e.Add(emu.DeviceSpec{MAC: mac, Model: "UGW3", IP: "192.168.1.242"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()

	c := emu.NewClassicClient(apiURL)
	if err := c.Login(ctx, "admin", "admin"); err != nil {
		t.Fatalf("Login: %v", err)
	}

	adoptAndWaitConnected(t, ctx, e, c, mac)
}

// fleetSpecs is the live fleet: exactly one gateway (the controller
// allows one per site), two switches, two access points. IPs are
// distinct but arbitrary — the controller never routes to them.
var fleetSpecs = []emu.DeviceSpec{
	{MAC: "00:27:22:e0:00:01", Model: "UGW3", IP: "192.168.1.242"},
	{MAC: "00:27:22:e0:00:11", Model: "USWED74", IP: "192.168.1.243"},
	{MAC: "00:27:22:e0:00:12", Model: "USM8P", IP: "192.168.1.244"},
	{MAC: "00:27:22:e0:00:21", Model: "U7MP", IP: "192.168.1.245"},
	{MAC: "00:27:22:e0:00:22", Model: "U7PRO", IP: "192.168.1.246"},
}

// TestEmuAdoptsFleetLive adopts the whole fleet. One live test per
// fresh controller: recreate the controller (scripts/itest.sh does
// this) and run with -run TestEmuAdoptsFleetLive; a combined
// `go test -tags integration .` run dies on the fresh-fixture check
// once TestEmuAdoptsUGWLive has adopted the gateway.
func TestEmuAdoptsFleetLive(t *testing.T) {
	informURL, apiURL := liveEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	e := emu.New(informURL)
	if err := e.Add(fleetSpecs...); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()

	c := emu.NewClassicClient(apiURL)
	if err := c.Login(ctx, "admin", "admin"); err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Serial adoption, one device fully connected before the next: the
	// controller build rejects bursts and too-fresh pending docs with
	// api.err.CannotAdopt (scripts/itest.sh has the live evidence).
	for _, spec := range fleetSpecs {
		adoptAndWaitConnected(t, ctx, e, c, spec.MAC)
	}
}

func liveEnv(t *testing.T) (informURL, apiURL string) {
	t.Helper()
	informURL = os.Getenv("UNIFI_EMU_TEST_INFORM_URL")
	apiURL = os.Getenv("UNIFI_EMU_TEST_API_URL")
	if informURL == "" || apiURL == "" {
		t.Skip("UNIFI_EMU_TEST_INFORM_URL/UNIFI_EMU_TEST_API_URL unset; skipping live controller test")
	}
	return informURL, apiURL
}

// adoptAndWaitConnected drives one device through the whole live flow:
// wait for the pending doc, adopt (retrying the rejections this
// controller build hands out for young docs), then wait for both the
// controller (state 1, adopted) and the emu (CONNECTED) to settle.
func adoptAndWaitConnected(t *testing.T, ctx context.Context, e *emu.Emu, c *emu.ClassicClient, mac string) {
	t.Helper()
	const site = "default"

	// The device must appear pending before the adopt click means
	// anything; the first inform lands within one interval of Start.
	pendingCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	var last emu.Device
	seen := false
	for {
		d, err := c.DeviceByMAC(pendingCtx, site, mac)
		if err == nil {
			last, seen = d, true
			if d.State == 1 && d.Adopted {
				t.Fatalf("%s already adopted before the test ran; recreate the controller fresh", mac)
			}
			if d.State == 2 {
				break
			}
		}
		select {
		case <-pendingCtx.Done():
			if seen {
				t.Fatalf("%s never appeared pending: %v (last state %d adopted=%v)",
					mac, pendingCtx.Err(), last.State, last.Adopted)
			}
			t.Fatalf("%s never appeared pending: %v (never listed, last error %v)", mac, pendingCtx.Err(), err)
		case <-time.After(2 * time.Second):
		}
	}

	// This controller build answers devmgr adopt with
	// api.err.CannotAdopt / api.err.CanNotAdoptUnknownDevice when the
	// pending doc is seconds old, and a failed attempt can reap the doc
	// (the emu's next inform re-creates it). Retry like a human
	// re-clicking Adopt; the doc's adopted flag is the source of truth.
	adoptCtx, stop := context.WithTimeout(ctx, 3*time.Minute)
	defer stop()
	for {
		err := c.Adopt(adoptCtx, site, mac)
		if err == nil {
			break
		}
		if !strings.Contains(strings.ToLower(err.Error()), "cannotadopt") {
			t.Fatalf("Adopt %s: %v", mac, err)
		}
		if d, derr := c.DeviceByMAC(adoptCtx, site, mac); derr == nil && d.Adopted {
			t.Logf("%s adopt returned CannotAdopt but the doc is already adopted; continuing", mac)
			break
		}
		t.Logf("%s adopt rejected (%v); retrying", mac, err)
		select {
		case <-adoptCtx.Done():
			t.Fatalf("%s never adopted: %v (last adopt error %v)", mac, adoptCtx.Err(), err)
		case <-time.After(10 * time.Second):
		}
	}

	waitAdoptedCtx, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	d, err := c.WaitAdopted(waitAdoptedCtx, site, mac)
	if err != nil {
		t.Fatalf("%s controller-side adoption: %v", mac, err)
	}
	t.Logf("%s controller: state=%d adopted=%v model=%s ip=%s name=%s",
		mac, d.State, d.Adopted, d.Model, d.IP, d.Name)

	waitCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if err := e.WaitState(waitCtx, mac, emu.StateConnected); err != nil {
		t.Fatalf("%s emu-side state: %v", mac, err)
	}
}
