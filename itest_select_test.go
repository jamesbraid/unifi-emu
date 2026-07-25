package emu_test

import (
	"math/rand"
	"net"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// seedFromEnv returns the fleet-selection seed: SIM_ITEST_SEED when set (so a
// CI failure reproduces exactly), otherwise a time-derived seed for variety.
func seedFromEnv() int64 {
	if s := os.Getenv("SIM_ITEST_SEED"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v
		}
	}
	return time.Now().UnixNano()
}

// selectFleet picks a reproducible pseudo-random fleet for the live adoption
// test: 2-3 distinct models in adoption order, including at most one gateway
// (a UniFi site adopts a single gateway, classic ugw or next-gen uxg). typeOf
// reports a model's device type ("ugw"/"uxg"/"usw"/"uap"); unknown models are
// skipped. The pool must hold at least two non-gateway models.
func selectFleet(all []string, typeOf func(string) (string, bool), seed int64) []string {
	r := rand.New(rand.NewSource(seed))
	var gateways, others []string
	for _, m := range all {
		t, ok := typeOf(m)
		if !ok {
			continue
		}
		// ugw and uxg are both gateway families; a site adopts one of
		// either, so they compete for the single gateway slot rather
		// than landing in the non-gateway pool.
		if t == "ugw" || t == "uxg" {
			gateways = append(gateways, m)
		} else {
			others = append(others, m)
		}
	}
	r.Shuffle(len(others), func(i, j int) { others[i], others[j] = others[j], others[i] })

	size := 2 + r.Intn(2) // 2 or 3 devices
	var pick []string
	if len(gateways) > 0 && r.Intn(2) == 0 { // at most one gateway, sometimes
		pick = append(pick, gateways[r.Intn(len(gateways))])
	}
	for _, m := range others {
		if len(pick) >= size {
			break
		}
		pick = append(pick, m)
	}
	r.Shuffle(len(pick), func(i, j int) { pick[i], pick[j] = pick[j], pick[i] })
	return pick
}

// macsForModels reproduces the emulator's SIM_MODELS expansion (mac = base +
// index + 1) so the caller knows which MACs to adopt.
func macsForModels(base string, n int) ([]string, error) {
	hw, err := net.ParseMAC(base)
	if err != nil {
		return nil, err
	}
	macs := make([]string, n)
	for i := range macs {
		macs[i] = advanceMAC(hw, i+1)
	}
	return macs, nil
}

func advanceMAC(base net.HardwareAddr, n int) string {
	v := uint64(0)
	for _, b := range base {
		v = v<<8 | uint64(b)
	}
	v += uint64(n)
	var mac [6]byte
	for i := 5; i >= 0; i-- {
		mac[i] = byte(v)
		v >>= 8
	}
	return net.HardwareAddr(mac[:]).String()
}

// --- unit tests (no container runtime needed) ---

func fakeTypeOf(types map[string]string) func(string) (string, bool) {
	return func(m string) (string, bool) { t, ok := types[m]; return t, ok }
}

func TestSelectFleetInvariants(t *testing.T) {
	types := map[string]string{
		"UGW3": "ugw", "UXGENT": "uxg", "USWED74": "usw", "USM8P": "usw",
		"U7MP": "uap", "U7PRO": "uap", "UAPA6B0": "uap",
	}
	all := []string{"UGW3", "UXGENT", "USWED74", "USM8P", "U7MP", "U7PRO", "UAPA6B0"}
	for seed := int64(0); seed < 200; seed++ {
		got := selectFleet(all, fakeTypeOf(types), seed)
		if len(got) < 2 || len(got) > 3 {
			t.Fatalf("seed %d: size %d, want 2-3: %v", seed, len(got), got)
		}
		seen := map[string]bool{}
		gateways := 0
		for _, m := range got {
			if seen[m] {
				t.Fatalf("seed %d: duplicate model %q in %v", seed, m, got)
			}
			seen[m] = true
			if types[m] == "" {
				t.Fatalf("seed %d: model %q not from pool", seed, m)
			}
			if types[m] == "ugw" || types[m] == "uxg" {
				gateways++
			}
		}
		if gateways > 1 {
			t.Fatalf("seed %d: %d gateways in %v, want <=1", seed, gateways, got)
		}
	}
}

func TestSelectFleetReproducible(t *testing.T) {
	types := map[string]string{"UGW3": "ugw", "USM8P": "usw", "U7PRO": "uap", "U7MP": "uap"}
	all := []string{"UGW3", "USM8P", "U7PRO", "U7MP"}
	a := selectFleet(all, fakeTypeOf(types), 12345)
	b := selectFleet(all, fakeTypeOf(types), 12345)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("same seed gave different fleets: %v vs %v", a, b)
	}
}

func TestMacsForModels(t *testing.T) {
	got, err := macsForModels("00:27:22:e0:00:00", 3)
	if err != nil {
		t.Fatalf("macsForModels: %v", err)
	}
	want := []string{"00:27:22:e0:00:01", "00:27:22:e0:00:02", "00:27:22:e0:00:03"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSeedFromEnvOverride(t *testing.T) {
	t.Setenv("SIM_ITEST_SEED", "42")
	if got := seedFromEnv(); got != 42 {
		t.Fatalf("seedFromEnv() = %d, want 42", got)
	}
}

func TestEmulatorModelsRequest(t *testing.T) {
	req := emulatorModelsRequest("net_abc", itestImages{emulator: "example/emu:test"}, "http://c:8080/inform", "UGW3,U7PRO")
	if req.Env["SIM_MODELS"] != "UGW3,U7PRO" {
		t.Fatalf("SIM_MODELS = %q, want UGW3,U7PRO", req.Env["SIM_MODELS"])
	}
	if req.Env["SIM_MAC_BASE"] != itestMACBase || req.Env["SIM_IP_BASE"] != itestIPBase {
		t.Fatalf("base envs = %q / %q", req.Env["SIM_MAC_BASE"], req.Env["SIM_IP_BASE"])
	}
	if req.Image != "example/emu:test" {
		t.Fatalf("Image = %q, want the prebuilt one", req.Image)
	}
}
