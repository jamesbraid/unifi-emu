package emu

import (
	"testing"

	"github.com/jamesbraid/unifi-emu/inform"
)

// The catalogue's power models exist to be claimed by a caller emulating a PDU
// or a UPS, so the outlet layout has to survive the whole path: profile, to
// descriptor, to payload. Before the catalogue carried outlets these models
// adopted as one-port switches wearing a PDU's name.
func TestRegistryPowerModelsReportOutlets(t *testing.T) {
	cases := []struct {
		model   string
		outlets int
		usb     int
	}{
		{"USPPDUP", 20, 4},
		{"USPRPSP", 1, 0},
		{"USPDA2B", 8, 0},
		{"USPDA2C", 9, 0},
		{"USWDA23", 10, 0},
		{"USWDA24", 10, 0},
		{"USWDA25", 8, 0},
		{"USWDA26", 8, 0},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			d := mustDevice(t, DeviceSpec{MAC: "02:00:00:00:00:01", Model: tc.model, IP: "10.0.0.99"})
			markAdopted(d)
			m := decodePayload(t, d)

			ot := table(t, m, "outlet_table")
			if len(ot) != tc.outlets {
				t.Fatalf("outlet_table has %d entries, want %d", len(ot), tc.outlets)
			}

			// Indexes are the join key the controller uses to match an
			// outlet to its override and its metering sample, so they
			// must be 1-based and contiguous.
			usb := 0
			for i, e := range ot {
				entry := e.(map[string]any)
				if entry["index"] != float64(i+1) {
					t.Errorf("entry %d has index %v, want %d", i, entry["index"], i+1)
				}
				caps := int(entry["outlet_caps"].(float64))
				_, hasType := entry["outlet_type"]
				_, hasRelay := entry["has_relay"]

				// Two encodings are in service, and a device speaks one
				// or the other. Mixing them leaves the controller
				// reading a field the model does not describe: it takes
				// a capability value below the AC class bit as the older
				// form and goes looking for an outlet_type, so an outlet
				// that omits one and understates the other describes
				// itself to nobody.
				if caps >= inform.OutletCapAC {
					if hasType {
						t.Errorf("outlet %d uses the class-bit encoding but also sends outlet_type", i+1)
					}
					if !hasRelay {
						t.Errorf("outlet %d uses the class-bit encoding but omits has_relay", i+1)
					}
					if caps&inform.OutletCapUSB != 0 {
						usb++
					}
					continue
				}
				if !hasType {
					t.Errorf("outlet %d uses the older encoding but sends no outlet_type", i+1)
				}
				if hasRelay {
					t.Errorf("outlet %d uses the older encoding but also sends has_relay", i+1)
				}
				if entry["outlet_type"] == float64(1) {
					usb++
				}
			}
			if usb != tc.usb {
				t.Errorf("got %d USB outlets, want %d", usb, tc.usb)
			}
		})
	}
}

// A sourced negative is a fact worth keeping: this model has no AC outlets at
// all, so an absent table is the correct report. A fabricated one would show
// outlets in the UI that the hardware does not have.
func TestRedundantPowerSupplyReportsNoOutlets(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "02:00:00:00:00:02", Model: "USPRPS", IP: "10.0.0.99"})
	markAdopted(d)
	m := decodePayload(t, d)
	if _, present := m["outlet_table"]; present {
		t.Errorf("outlet_table present on a model with no outlets: %v", m["outlet_table"])
	}
}

// The battery-backed models are the ones the controller can render as a UPS,
// and the NUT bit is what unlocks that. Without it they are power strips with
// batteries the UI never shows.
func TestBatteryBackedModelsClaimTheUPSCapability(t *testing.T) {
	for _, model := range []string{"USPDA2B", "USPDA2C", "USWDA23", "USWDA24", "USWDA25", "USWDA26"} {
		t.Run(model, func(t *testing.T) {
			d := mustDevice(t, DeviceSpec{MAC: "02:00:00:00:00:03", Model: model, IP: "10.0.0.99"})
			markAdopted(d)
			m := decodePayload(t, d)

			caps, ok := m["smart_power_caps"].(float64)
			if !ok {
				t.Fatalf("smart_power_caps missing on a battery-backed model")
			}
			if int(caps)&inform.SmartPowerCapNUTInformationAccess == 0 {
				t.Errorf("smart_power_caps = %d, missing the capability that makes it a UPS", int(caps))
			}
			if _, present := m["vbms_table"]; !present {
				t.Errorf("vbms_table missing on a model claiming the UPS capability")
			}
		})
	}
}

// The spec-level outlet override mirrors the port override: a caller driving a
// synthetic fleet can ask for an N-outlet power device without the catalogue
// carrying that exact model.
func TestSpecOutletCountOverridesTheProfile(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "02:00:00:00:00:04", Model: "USPPDUP", IP: "10.0.0.99", Outlets: 3})
	markAdopted(d)
	m := decodePayload(t, d)
	if got := len(table(t, m, "outlet_table")); got != 3 {
		t.Errorf("outlet_table has %d entries, want the 3 the spec asked for", got)
	}
}
