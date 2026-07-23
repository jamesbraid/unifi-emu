package emu

import "testing"

func TestGeneratedModelRegistryMatchesControllerMetadata(t *testing.T) {
	wantPorts := map[string]int{
		"UGW3": 3, "USWED74": 4, "USM8P": 8, "US48P750": 52,
		"USWED06": 16, "USWF07D": 32, "U7MP": 2, "U7PRO": 1, "UAPA6B0": 1,
	}
	if len(modelRegistry) != len(wantPorts) {
		t.Fatalf("model registry has %d models, want %d", len(modelRegistry), len(wantPorts))
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
	if got := modelRegistry["USM8P"].Ports[7].Media; got != "GE" {
		t.Errorf("USM8P port 8 media = %q, want GE", got)
	}
	if got := modelRegistry["U7PRO"].Ports[0].Media; got != "2.5GbE" {
		t.Errorf("U7PRO uplink media = %q, want 2.5GbE", got)
	}
	if got := modelRegistry["U7MP"].Radios[0].NSS; got != 3 {
		t.Errorf("U7MP radio NSS = %d, want 3", got)
	}
	if got := modelRegistry["USWF07D"].Ports[31].Media; got != "QSFP28" {
		t.Errorf("USWF07D port 32 media = %q, want QSFP28", got)
	}
}
