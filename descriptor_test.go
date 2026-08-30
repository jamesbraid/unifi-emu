package emu

import (
	"testing"

	"github.com/jamesbraid/unifi-emu/inform"
)

func TestBuildDescriptorResolvesSerial(t *testing.T) {
	got := buildDescriptor(DeviceSpec{MAC: "dc:9f:db:00:00:01", Model: "UGW3"}, modelRegistry["UGW3"])
	if got.Serial != "DC9FDB000001" {
		t.Errorf("Serial = %q, want MAC-derived DC9FDB000001", got.Serial)
	}
	got = buildDescriptor(DeviceSpec{MAC: "dc:9f:db:00:00:01", Model: "UGW3", Serial: "F09FC2ABCDEF"}, modelRegistry["UGW3"])
	if got.Serial != "F09FC2ABCDEF" {
		t.Errorf("Serial = %q, want the supplied serial", got.Serial)
	}
}

func TestBuildDescriptorResolvesFWCaps(t *testing.T) {
	zero := 0
	got := buildDescriptor(DeviceSpec{MAC: "00:27:22:e0:00:31", Model: "U7MP", FWCaps: &zero}, modelRegistry["U7MP"])
	if got.FWCaps != 0 {
		t.Errorf("FWCaps = %d, want an explicit 0 to survive", got.FWCaps)
	}
	got = buildDescriptor(DeviceSpec{MAC: "00:27:22:e0:00:31", Model: "U7MP"}, modelRegistry["U7MP"])
	if got.FWCaps != inform.PlaceholderFWCaps {
		t.Errorf("FWCaps = %d, want the placeholder %d", got.FWCaps, inform.PlaceholderFWCaps)
	}
}

func TestBuildDescriptorPortOverride(t *testing.T) {
	got := buildDescriptor(DeviceSpec{MAC: "00:27:22:00:00:02", Model: "USWED74", Ports: 3}, modelRegistry["USWED74"])
	if len(got.Ports) != 3 {
		t.Fatalf("Ports = %d, want 3 from the override", len(got.Ports))
	}
	if !got.Ports[0].IsUplink || got.Ports[1].IsUplink {
		t.Errorf("only port 1 should be the uplink: %+v", got.Ports)
	}
}
