package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testCaps is the vocabulary the harvest tests resolve names against: the
// real UDAPI values, so a test that claims a bit gets the bit a controller
// would gate on.
func testCaps() capsFile {
	return capsFile{
		ControllerVersion: "10.4.57",
		Bitmaps: map[string]capBitmap{
			udapiFamily: {
				Field: "udapi_caps",
				Bits: map[string]int{
					"UNIFI_UDAPI_CAP_ROUTES_BGP":    1 << 22,
					"UNIFI_UDAPI_CAP_ROUTES_OSPFV2": 1 << 18,
				},
			},
		},
	}
}

func TestUdapiMaskORsNamedBits(t *testing.T) {
	caps := testCaps()
	got, err := caps.udapiMask("UXGENT", []string{
		"UNIFI_UDAPI_CAP_ROUTES_BGP", "UNIFI_UDAPI_CAP_ROUTES_OSPFV2",
	})
	if err != nil {
		t.Fatalf("udapiMask: %v", err)
	}
	if want := 1<<22 | 1<<18; got != want {
		t.Errorf("mask = %d, want %d", got, want)
	}
	if got, err := caps.udapiMask("UXGENT", nil); err != nil || got != 0 {
		t.Errorf("empty caps = %d, %v; want 0, nil", got, err)
	}
}

// A misspelled bit must stop the build. Skipping it would emit a device
// claiming nothing while the override says it claims something -- the same
// silent under-reporting the udapi_caps work exists to fix.
func TestUdapiMaskRejectsUnknownBit(t *testing.T) {
	_, err := testCaps().udapiMask("UXGENT", []string{"UNIFI_UDAPI_CAP_ROUTES_BGB"})
	if err == nil {
		t.Fatal("unknown capability name accepted")
	}
	if !strings.Contains(err.Error(), "UNIFI_UDAPI_CAP_ROUTES_BGB") ||
		!strings.Contains(err.Error(), "UXGENT") {
		t.Errorf("error does not name the model and the bad bit: %v", err)
	}
}

// The checked-in vocabulary must carry the bit the catalog claims: a
// regenerated capability_bits.json that lost it would otherwise fail far
// away, in a live adoption.
func TestCheckedInCapsCarryTheClaimedBits(t *testing.T) {
	path := filepath.Join("..", "..", "capability_bits.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no %s: %v", path, err)
	}
	caps, err := loadCaps(path)
	if err != nil {
		t.Fatalf("loadCaps: %v", err)
	}
	ov, err := loadOverrides(filepath.Join("..", "..", "model_overrides.json"))
	if err != nil {
		t.Fatalf("loadOverrides: %v", err)
	}
	for model, o := range ov.Models {
		if o.UDAPI == nil {
			continue
		}
		if o.UDAPI.Version == "" {
			t.Errorf("%s: udapi override has no version; the controller drops a bitmap sent without one", model)
		}
		mask, err := caps.udapiMask(model, o.UDAPI.Caps)
		if err != nil {
			t.Errorf("%s: %v", model, err)
			continue
		}
		if mask == 0 {
			t.Errorf("%s: udapi override resolves to an empty bitmap", model)
		}
	}
}
