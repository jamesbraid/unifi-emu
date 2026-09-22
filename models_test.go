package emu

import "testing"

func TestGeneratedModelRegistryMatchesControllerMetadata(t *testing.T) {
	wantPorts := map[string]int{
		"UGW3": 3, "USWED74": 4, "USM8P": 8, "US48P750": 52,
		"USWED06": 16, "USWF07D": 32, "U7MP": 2, "U7PRO": 1, "UAPA6B0": 1,
		// The hardware DB gives this one six SFP+ uplinks; the shipped
		// USW-Enterprise-48-PoE has four, and an override says so.
		"US648P": 52,
	}
	// Measured never to reach a controller document, so they must not be
	// emulatable: see excludedModels in cmd/modelgen for the per-model
	// evidence. Named here because the count below would not say which model
	// came back, and a resurrected one costs a live run a silent timeout.
	for _, model := range []string{"UGWHD4", "UAPA6BE", "UAPA6BF", "U7E", "U7O"} {
		if _, ok := modelRegistry[model]; ok {
			t.Errorf("%s is back in the registry; no controller lists it", model)
		}
	}

	// An exact count, not a floor: a regeneration that silently drops models
	// is the failure this guards, and a deliberate change to the lineup is
	// exactly when the number should be reviewed rather than tolerated.
	const wantModels = 183
	if len(modelRegistry) != wantModels {
		t.Fatalf("model registry has %d models, want %d (the full lineup at controller 10.6.106, "+
			"minus the exclusions in cmd/modelgen)", len(modelRegistry), wantModels)
	}
	for model, portCount := range wantPorts {
		profile, ok := modelRegistry[model]
		if !ok {
			t.Errorf("model registry is missing %s", model)
			continue
		}
		if profile.Model != model || profile.ModelDisplay == "" ||
			profile.Type == "" || profile.Version == "" {
			t.Errorf("%s has incomplete identity: %+v", model, profile)
		}
		if len(profile.Ports) != portCount {
			t.Errorf("%s has %d ports, want %d", model, len(profile.Ports), portCount)
		}
		for i, port := range profile.Ports {
			if port.PortIdx != i+1 || port.IfName == "" || port.Name == "" || port.Media == "" {
				t.Errorf("%s port %d is incomplete or out of order: %+v", model, i, port)
			}
		}
	}

	if got := len(modelRegistry["U7PRO"].Radios); got != 3 {
		t.Errorf("U7PRO has %d radios, want ng, na, and 6e", got)
	}
	for _, model := range []string{"U7MP", "UAPA6B0"} {
		if got := len(modelRegistry[model].Radios); got != 2 {
			t.Errorf("%s has %d radios, want ng and na", model, got)
		}
	}
	if got := modelRegistry["US48P750"].Ports[48].Media; got != "SFP+" {
		t.Errorf("US48P750 port 49 media = %q, want SFP+", got)
	}
	if got := modelRegistry["US48P750"].Ports[50].Media; got != "SFP" {
		t.Errorf("US48P750 port 51 media = %q, want SFP", got)
	}
	// All three Ultras file the PoE++ input under the hardware DB's "plus"
	// category; none of them has an SFP+ cage.
	for _, model := range []string{"USM8P", "USM8P60", "USM8P210"} {
		if got := modelRegistry[model].Ports[7].Media; got != "GE" {
			t.Errorf("%s port 8 media = %q, want GE", model, got)
		}
	}
	// The ECS-24S pair's 24 access ports are RJ45, and the hardware DB calls
	// them SFP+. The bank is split by speed -- eight at 2.5G, sixteen at
	// 10G -- which the source describes as one gigabit category. Only the
	// PoE SKU powers them, and it powers them at those speeds: PoE follows
	// copper, not gigabit specifically.
	for model, wantPoE := range map[string]int{"USWF004": 7, "USWF005": 0} {
		ports := modelRegistry[model].Ports
		if got := ports[0].Media; got != "2.5GbE" {
			t.Errorf("%s port 1 media = %q, want 2.5GbE", model, got)
		}
		if got := ports[8].Media; got != "10GbE" {
			t.Errorf("%s port 9 media = %q, want 10GbE", model, got)
		}
		if got := ports[0].PoECaps; got != wantPoE {
			t.Errorf("%s port 1 poe_caps = %d, want %d", model, got, wantPoE)
		}
	}
	// A switch that powers only some of its ports says which. The US-8's
	// single output and the Ultra's rear input are the two shapes that go
	// wrong in opposite directions: one under-powers a bank, the other
	// describes a port that takes power as one that gives it.
	for model, want := range map[string][]int{
		"US8":    {8},
		"USF5P":  {2, 3, 4, 5},
		"USM8P":  {1, 2, 3, 4, 5, 6, 7},
		"USL24P": {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
	} {
		var powered []int
		for _, p := range modelRegistry[model].Ports {
			if p.PoECaps != 0 {
				powered = append(powered, p.PortIdx)
			}
		}
		if len(powered) != len(want) {
			t.Errorf("%s powers %v, want %v", model, powered, want)
			continue
		}
		for i := range want {
			if powered[i] != want[i] {
				t.Errorf("%s powers %v, want %v", model, powered, want)
				break
			}
		}
	}
	// The non-PoE Pro Max 48 inherits its PoE sibling's PoE flag upstream.
	if got := modelRegistry["USPM48"].Ports[0].PoECaps; got != 0 {
		t.Errorf("USPM48 port 1 poe_caps = %d, want 0 (the PSE is on USPM48P)", got)
	}
	if got := modelRegistry["U7PRO"].Ports[0].Media; got != "2.5GbE" {
		t.Errorf("U7PRO uplink media = %q, want 2.5GbE", got)
	}
	if got := modelRegistry["U7MP"].Radios[0].NSS; got != 3 {
		t.Errorf("U7MP radio NSS = %d, want 3", got)
	}
	// The hardware DB carries no stream count, so every radio derives as
	// 2x2 and the rest come from overrides. These three are one per width,
	// picked so a dropped override cannot pass unnoticed; U7PROMAX also
	// pins that the widths differ per band on the same device.
	for model, want := range map[string][]int{
		"U2IW":     {1},
		"U7PROMAX": {2, 4, 2},
		"U7HD":     {4, 4},
	} {
		radios := modelRegistry[model].Radios
		if len(radios) != len(want) {
			t.Errorf("%s has %d radios, want %d", model, len(radios), len(want))
			continue
		}
		for i, nss := range want {
			if radios[i].NSS != nss {
				t.Errorf("%s %s NSS = %d, want %d", model, radios[i].Radio, radios[i].NSS, nss)
			}
		}
	}
	// hw_caps as real units report it. Each of these was captured from the
	// model on its shipping firmware; the derivation alone knows only the
	// outlet bit, so a dropped override reads as hardware going missing.
	for model, want := range map[string]int{
		"USPPDUP":  136,  // outlet + LCM
		"USAGGPRO": 24,   // LCM + RPS
		"US648P":   24,   // LCM + RPS
		"USL8A":    8,    // LCM
		"USF5P":    8192, // takes 802.3bt type 3
		"U6ENT":    4608, // takes 802.3at; accelerometer
		"U7PG2":    2048, // takes 802.3af
		"UAP6MP":   2562, // takes 802.3af; accelerometer; LED bar
	} {
		if got := modelRegistry[model].HWCaps; got != want {
			t.Errorf("%s hw_caps = %d, want %d", model, got, want)
		}
	}
	if got := modelRegistry["USWF07D"].Ports[31].Media; got != "QSFP28" {
		t.Errorf("USWF07D port 32 media = %q, want QSFP28", got)
	}
}
