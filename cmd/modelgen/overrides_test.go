package main

import "testing"

func TestApplyOverrideEth(t *testing.T) {
	m := &catalogModel{Model: "U7PRO", Type: "uap"}
	applyOverride(m, modelOverride{Eth: &ethOverride{Count: 1, Media: "2.5GbE"}})
	if len(m.Ports) != 1 || m.Ports[0].Media != "2.5GbE" {
		t.Fatalf("eth override not applied: %+v", m.Ports)
	}
}

func TestStaleOverrideRejected(t *testing.T) {
	ov := overrides{Models: map[string]modelOverride{"GHOST": {}}}
	if err := checkStaleOverrides(ov, map[string]bool{"U7PRO": true}); err == nil {
		t.Fatal("stale override accepted, want error")
	}
}
