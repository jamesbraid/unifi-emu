//go:build integration

package emu_test

import (
	"context"
	"os"
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
// from this host; scripts/itest.sh has the details.
func TestEmuAdoptsUGWLive(t *testing.T) {
	informURL := os.Getenv("UNIFI_EMU_TEST_INFORM_URL")
	apiURL := os.Getenv("UNIFI_EMU_TEST_API_URL")
	if informURL == "" || apiURL == "" {
		t.Skip("UNIFI_EMU_TEST_INFORM_URL/UNIFI_EMU_TEST_API_URL unset; skipping live controller test")
	}
	mac := os.Getenv("UNIFI_EMU_TEST_MAC")
	if mac == "" {
		mac = "00:27:22:e0:00:01"
	}
	const site = "default"

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
				t.Fatalf("device already adopted before the test ran; recreate the controller fresh")
			}
			if d.State == 2 {
				break
			}
		}
		select {
		case <-pendingCtx.Done():
			if seen {
				t.Fatalf("device never appeared pending: %v (last state %d adopted=%v)",
					pendingCtx.Err(), last.State, last.Adopted)
			}
			t.Fatalf("device never appeared pending: %v (never listed, last error %v)", pendingCtx.Err(), err)
		case <-time.After(2 * time.Second):
		}
	}

	if err := c.Adopt(ctx, site, mac); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	d, err := c.WaitAdopted(ctx, site, mac)
	if err != nil {
		t.Fatalf("controller-side adoption: %v", err)
	}
	t.Logf("controller: state=%d adopted=%v model=%s ip=%s name=%s",
		d.State, d.Adopted, d.Model, d.IP, d.Name)

	waitCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if err := e.WaitState(waitCtx, mac, emu.StateConnected); err != nil {
		t.Fatalf("emu-side state: %v", err)
	}
}
