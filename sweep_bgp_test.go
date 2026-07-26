//go:build integration

package emu_test

// The BGP probe measures what the catalog only claims. Which models have the
// UDAPI routing capability comes from Ubiquiti's published support matrix
// (see the UXGENT entry in model_overrides.json), and the controller believes
// whatever a device reports -- hasUdapiCapability is a plain bitmask read. So
// nothing in the emulator or the controller can tell us the claim is right;
// only asking a controller to configure BGP against an adopted device can.
//
// It rides the adoption sweep because that is where adopted gateways already
// exist. To measure only the gateways, restrict the sweep to them:
//
//	UNIFI_EMU_ITEST_SWEEP=1 \
//	UNIFI_EMU_ITEST_SWEEP_MODELS=UXGENT,UXG,UXGB,UXGA6AA,UXGPRO,UGW3,UGW4,UGWXG \
//	go test -tags integration -run TestClassicCatalogAdoptionSweepLive -v -count=1 .

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	emu "github.com/jamesbraid/unifi-emu"
)

// udapiCapRoutesBGP is UNIFI_UDAPI_CAP_ROUTES_BGP from capability_bits.json,
// the bit the controller reads to decide whether a device can do BGP.
const udapiCapRoutesBGP = 1 << 22

// The three answers a probe can get, recorded in the sweep report.
const (
	// bgpAccepted means the controller took the config: the device is
	// capable as far as the controller is concerned.
	bgpAccepted = "accepted"
	// bgpUnsupported is the capability refusal -- 404 with
	// api.err.BgpUnsupportedDevice, what every gateway answered before any
	// of them reported udapi_caps.
	bgpUnsupported = "unsupported"
	// bgpProbeFailed is anything else, and is not evidence either way.
	bgpProbeFailed = "probe-failed"
)

// bgpUnsupportedKey is the controller's own error key for the capability
// refusal (com.ubnt.service.bgp: "Device doesn't support BGP"). Matching it
// rather than the bare 404 keeps a missing route or a renamed endpoint from
// reading as a capable device correctly refusing.
const bgpUnsupportedKey = "api.err.BgpUnsupportedDevice"

// probeBGP asks the controller to store a BGP config for an adopted gateway
// and reports which of the three answers came back.
//
// The body's field names are the controller's own wire names for the
// bgp_router document (device_mac, enabled, frr_bgpd_config,
// uploaded_file_name). The config text only has to be something the
// controller will store; this probe is about whether the endpoint refuses
// the device, not about FRR syntax.
func probeBGP(ctx context.Context, client bgpProber, site, mac string) (string, string) {
	status, body, err := client.PostRaw(ctx, "/v2/api/site/"+site+"/bgp/config", map[string]any{
		"device_mac":         mac,
		"enabled":            true,
		"frr_bgpd_config":    "router bgp 65000\n",
		"uploaded_file_name": "sweep-probe.conf",
	})
	if err != nil {
		return bgpProbeFailed, err.Error()
	}
	detail := strings.TrimSpace(string(body))
	if len(detail) > 200 {
		detail = detail[:200]
	}
	switch {
	case status == http.StatusOK:
		return bgpAccepted, ""
	case strings.Contains(string(body), bgpUnsupportedKey):
		return bgpUnsupported, ""
	default:
		return bgpProbeFailed, fmt.Sprintf("HTTP %d: %s", status, detail)
	}
}

// bgpProber is the slice of the adopt client the probe needs.
type bgpProber interface {
	PostRaw(ctx context.Context, path string, payload any) (int, []byte, error)
}

// probeGatewayBGP fills in a gateway outcome's BGP result. Non-gateways and
// devices that never adopted are left blank: the endpoint is meaningless
// without an adopted gateway, and a blank field says "not measured" rather
// than implying a refusal.
func probeGatewayBGP(t *testing.T, ctx context.Context, client bgpProber, outcome *sweepOutcome) {
	t.Helper()
	if !outcome.Adopted || (outcome.Type != "ugw" && outcome.Type != "uxg") {
		return
	}
	result, detail := probeBGP(ctx, client, sweepSite, outcome.MAC)
	outcome.BGP = result

	// A probe that neither succeeded nor hit the capability refusal says
	// nothing about the device: an endpoint this controller build does not
	// serve looks the same from here as one that does. Record and move on.
	if result == bgpProbeFailed {
		outcome.Note = strings.TrimSpace(outcome.Note + " bgp probe inconclusive: " + detail)
		t.Logf("%s: bgp probe inconclusive (%s) -- not evidence either way", outcome.Model, detail)
		return
	}

	// A model that claims the capability must be taken up on it, and one
	// that does not must be refused. Either mismatch means the catalog and
	// the controller disagree about the hardware, which is what this probe
	// exists to catch.
	want := bgpUnsupported
	if claimsBGP(outcome.Model) {
		want = bgpAccepted
	}
	if result != want {
		t.Errorf("%s: BGP %s, want %s -- catalog claim and controller disagree",
			outcome.Model, result, want)
	}
}

// claimsBGP reports whether the catalog has this model reporting the routing
// capability, read from the profile the emulator actually informs with.
func claimsBGP(model string) bool {
	profile, ok := emu.Profile(model)
	if !ok {
		return false
	}
	return profile.UDAPICaps&udapiCapRoutesBGP != 0
}
