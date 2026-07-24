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

// adopter is the controller-side adopt surface shared by ClassicClient and
// UOSClient, so adoptAndWaitConnected drives either dialect: classic (:8443
// cookie) or UniFi-OS-native (:443 CSRF over /proxy/network).
type adopter interface {
	Adopt(ctx context.Context, site, mac string) error
	DeviceByMAC(ctx context.Context, site, mac string) (emu.Device, error)
	WaitAdopted(ctx context.Context, site, mac string) (emu.Device, error)
}

// TestEmuAdoptsUOSLive proves the full UniFi-OS-native path end to end: an
// emulated device informs in, then UOSClient logs in on :443, follows the
// ucore CSRF-token rotation, and adopts through /proxy/network — against the
// seeded UOS image, which ships no demo devices so the emu is the only
// pending device. Boot the published seeded image with the documented
// runtime contract, and --no-healthcheck (its healthcheck logs in every 10s
// and burns the global login rate limit — see docs/UOS-SEEDED-FINDINGS.md):
//
//	run-uos.sh uos-seeded ghcr.io/jamesbraid/unifi-os-server:seeded \
//	  --no-healthcheck -p 11443:443 -p 18080:8080
//
// then, once the seed-owner has set up the :443 API:
//
//	UNIFI_EMU_TEST_UOS_INFORM_URL=http://127.0.0.1:18080/inform \
//	UNIFI_EMU_TEST_UOS_API_URL=https://localhost:11443 \
//	go test -tags integration -run TestEmuAdoptsUOSLive -v .
//
// Separate env vars from the classic tests so a combined run drives each
// dialect against its own fresh controller, not the wrong one.
func TestEmuAdoptsUOSLive(t *testing.T) {
	informURL := os.Getenv("UNIFI_EMU_TEST_UOS_INFORM_URL")
	apiURL := os.Getenv("UNIFI_EMU_TEST_UOS_API_URL")
	if informURL == "" || apiURL == "" {
		t.Skip("UNIFI_EMU_TEST_UOS_INFORM_URL/UNIFI_EMU_TEST_UOS_API_URL unset; skipping live UOS test")
	}
	user := os.Getenv("UNIFI_EMU_TEST_UOS_USER")
	if user == "" {
		user = "admin"
	}
	pass := os.Getenv("UNIFI_EMU_TEST_UOS_PASS")
	if pass == "" {
		pass = "admin"
	}
	mac := os.Getenv("UNIFI_EMU_TEST_UOS_MAC")
	if mac == "" {
		mac = "00:27:22:e0:00:31"
	}

	// Generous: UOS login rate-limiting can cost a few minutes of backoff
	// before a slot opens, on top of the adoption handshake itself.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	e := emu.New(informURL)
	if err := e.Add(emu.DeviceSpec{MAC: mac, Model: "U7PRO", IP: "192.168.1.247"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()

	c := emu.NewUOSClient(apiURL)
	if err := c.Login(ctx, user, pass); err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Assert adoption, not full "connected": on this seeded image's newer
	// bundled Network app the controller parks an upgraded device at state=4
	// (it re-issues a new cfgversion every inform and never clears
	// wait_for_initial_inform), so state never reaches 1. Adoption itself
	// completes — that is what the :443 path proves here. The unresolved
	// provisioning issue is documented in docs/UOS-SEEDED-FINDINGS.md.
	driveAdopt(t, ctx, c, mac)

	adoptedCtx, stopA := context.WithTimeout(ctx, 90*time.Second)
	defer stopA()
	d := waitControllerAdopted(t, adoptedCtx, c, mac)
	t.Logf("%s controller: adopted=%v state=%d (state 4 = upgrading is a known seeded-image limitation)",
		mac, d.Adopted, d.State)

	waitCtx, stopW := context.WithTimeout(ctx, 30*time.Second)
	defer stopW()
	if err := e.WaitState(waitCtx, mac, emu.StateConnected); err != nil {
		t.Fatalf("%s emu-side state: %v", mac, err)
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

// driveAdopt drives one device from pending to controller-adopted: wait for
// the pending doc, then adopt, retrying the young-doc rejections this
// controller build hands out. It returns once the adopt command is accepted;
// the caller decides which post-adoption state to require.
func driveAdopt(t *testing.T, ctx context.Context, c adopter, mac string) {
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
}

// adoptAndWaitConnected drives a device all the way to connected: adopt, then
// wait for both the controller (state 1, adopted) and the emu (CONNECTED).
// For controllers that finalize provisioning to state 1 (the classic Network
// app sim).
func adoptAndWaitConnected(t *testing.T, ctx context.Context, e *emu.Emu, c adopter, mac string) {
	t.Helper()
	const site = "default"
	driveAdopt(t, ctx, c, mac)

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

// waitControllerAdopted polls until the controller reports the device
// adopted, regardless of the numeric device state. The seeded UOS image's
// newer bundled Network app parks an upgraded device at state=4 and never
// advances it to state=1 (see docs/UOS-SEEDED-FINDINGS.md), so a state==1
// wait would time out on an adoption that actually succeeded.
func waitControllerAdopted(t *testing.T, ctx context.Context, c adopter, mac string) emu.Device {
	t.Helper()
	const site = "default"
	var last emu.Device
	for {
		if d, err := c.DeviceByMAC(ctx, site, mac); err == nil {
			last = d
			if d.Adopted {
				return d
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s never reported adopted: %v (last state %d adopted=%v)",
				mac, ctx.Err(), last.State, last.Adopted)
			return last
		case <-time.After(2 * time.Second):
		}
	}
}
