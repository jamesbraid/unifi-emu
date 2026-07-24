package emu

import "testing"

// TestLoadRegistryDerivesRadioChannel guards the one thing the embed+parse
// step can't get from model_profiles.json itself: each radio's channel,
// which loadRegistry derives from the radio band via defaultChannel.
func TestLoadRegistryDerivesRadioChannel(t *testing.T) {
	if len(modelRegistry) <= 150 {
		t.Fatalf("model registry has %d models, want more than 150", len(modelRegistry))
	}

	profile, ok := modelRegistry["U7PRO"]
	if !ok {
		t.Fatal("model registry is missing U7PRO")
	}
	if len(profile.Radios) == 0 {
		t.Fatal("U7PRO has no radios")
	}
	if got := profile.Radios[0].Radio; got != "ng" {
		t.Fatalf("U7PRO radio[0] band = %q, want ng", got)
	}
	if got := profile.Radios[0].Channel; got != 1 {
		t.Errorf("U7PRO ng radio channel = %d, want 1", got)
	}

	var foundNA bool
	for _, r := range profile.Radios {
		if r.Radio != "na" {
			continue
		}
		foundNA = true
		if r.Channel != 36 {
			t.Errorf("U7PRO na radio channel = %d, want 36", r.Channel)
		}
	}
	if !foundNA {
		t.Fatal("U7PRO has no na radio")
	}

	var found6E bool
	for _, r := range profile.Radios {
		if r.Radio != "6e" {
			continue
		}
		found6E = true
		if r.Channel != 5 {
			t.Errorf("U7PRO 6e radio channel = %d, want 5", r.Channel)
		}
	}
	if !found6E {
		t.Fatal("U7PRO has no 6e radio")
	}
}
