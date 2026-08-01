package herder

import (
	"reflect"
	"strings"
	"testing"
)

// mappedConfig maps model to the fixture image reference.
func mappedConfig(models ...string) RuntimeConfig {
	cfg := RuntimeConfig{Version: 1, Models: map[string]ModelRuntime{}}
	for _, m := range models {
		cfg.Models[m] = ModelRuntime{Image: "fixture.example/" + strings.ToLower(m) + "@" + testDigest}
	}
	return cfg
}

func ids(models ...string) []Identity {
	out := make([]Identity, len(models))
	for i, m := range models {
		out[i] = Identity{Index: i, Model: m, MAC: "02:00:00:00:00:0" + string(rune('0'+i)),
			Serial: "EMU0", Name: "emu-x"}
	}
	return out
}

func buildPlan(t *testing.T, identities []Identity, cfg RuntimeConfig, syntheticImage string) Plan {
	t.Helper()
	plan, err := BuildPlan(PlanInput{
		RunID: "run1", Network: "net", InformURL: "http://172.28.0.2:8080/inform",
		Devices: identities, Runtime: cfg, SyntheticImage: syntheticImage,
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return plan
}

func TestPlanBatchesEveryUnmappedDeviceIntoOneSyntheticUnit(t *testing.T) {
	plan := buildPlan(t, ids("USM8P", "UGW3", "U7PRO"), RuntimeConfig{}, "public/emu:1.0.0")
	if len(plan.Units) != 1 {
		t.Fatalf("units = %d, want one synthetic batch", len(plan.Units))
	}
	unit := plan.Units[0]
	if unit.Kind != UnitSynthetic {
		t.Fatalf("unit kind = %v, want synthetic", unit.Kind)
	}
	if unit.Image != "public/emu:1.0.0" {
		t.Fatalf("image = %q", unit.Image)
	}
	if len(unit.Devices) != 3 {
		t.Fatalf("synthetic unit holds %d devices, want 3", len(unit.Devices))
	}
	if unit.DeviceIndices() != "0,1,2" {
		t.Fatalf("device indices = %q, want 0,1,2", unit.DeviceIndices())
	}
}

func TestPlanGivesEachMappedDeviceItsOwnUnit(t *testing.T) {
	plan := buildPlan(t, ids("USM8P", "UXGENT", "UXGENT"), mappedConfig("UXGENT"), "public/emu:1.0.0")
	if len(plan.Units) != 3 {
		t.Fatalf("units = %d, want one synthetic plus two opaque", len(plan.Units))
	}
	if plan.Units[0].Kind != UnitSynthetic || len(plan.Units[0].Devices) != 1 {
		t.Fatalf("unit 0 = %#v, want the synthetic batch with device 0", plan.Units[0])
	}
	for _, i := range []int{1, 2} {
		if plan.Units[i].Kind != UnitOpaque {
			t.Fatalf("unit %d kind = %v, want opaque", i, plan.Units[i].Kind)
		}
		if len(plan.Units[i].Devices) != 1 {
			t.Fatalf("unit %d holds %d devices, want exactly 1", i, len(plan.Units[i].Devices))
		}
	}
	// Two devices of the same model get two separate containers, not one.
	if plan.Units[1].Devices[0].Index == plan.Units[2].Devices[0].Index {
		t.Fatal("both opaque units carry the same device")
	}
}

func TestPlanKeepsUnitsInRequestOrder(t *testing.T) {
	plan := buildPlan(t, ids("UXGENT", "USM8P"), mappedConfig("UXGENT"), "public/emu:1.0.0")
	if plan.Units[0].Kind != UnitOpaque || plan.Units[0].Devices[0].Index != 0 {
		t.Fatalf("unit 0 = %#v, want the opaque device 0 first", plan.Units[0])
	}
	if plan.Units[1].Kind != UnitSynthetic || plan.Units[1].Devices[0].Index != 1 {
		t.Fatalf("unit 1 = %#v, want the synthetic batch second", plan.Units[1])
	}
}

func TestPlanWithNoUnmappedDeviceNeedsNoSyntheticImage(t *testing.T) {
	plan := buildPlan(t, ids("UXGENT"), mappedConfig("UXGENT"), "")
	if len(plan.Units) != 1 || plan.Units[0].Kind != UnitOpaque {
		t.Fatalf("units = %#v, want a single opaque unit", plan.Units)
	}
}

func TestPlanRequiresASyntheticImageWhenItHasSyntheticDevices(t *testing.T) {
	_, err := BuildPlan(PlanInput{
		RunID: "run1", Network: "net", InformURL: "http://172.28.0.2:8080/inform",
		Devices: ids("USM8P"), Runtime: RuntimeConfig{}, SyntheticImage: "",
	})
	assertFailure(t, err, CodeSyntheticImageRequired, PhaseValidate)
}

// Every requested model must exist in the public synthetic registry, even
// when an opaque runtime is mapped for it: that is what keeps one request
// running all-synthetic on a runner with no mapping installed.
func TestPlanRejectsAModelWithNoSyntheticProfile(t *testing.T) {
	_, err := BuildPlan(PlanInput{
		RunID: "run1", Network: "net", InformURL: "http://172.28.0.2:8080/inform",
		Devices: ids("NOT-A-REAL-MODEL"), Runtime: RuntimeConfig{}, SyntheticImage: "public/emu:1.0.0",
	})
	assertFailure(t, err, CodeSyntheticModelUnknown, PhaseValidate)
}

func TestPlanRejectsAMappedModelWithNoSyntheticProfile(t *testing.T) {
	_, err := BuildPlan(PlanInput{
		RunID: "run1", Network: "net", InformURL: "http://172.28.0.2:8080/inform",
		Devices: ids("NOT-A-REAL-MODEL"), Runtime: mappedConfig("NOT-A-REAL-MODEL"),
		SyntheticImage: "public/emu:1.0.0",
	})
	assertFailure(t, err, CodeSyntheticModelUnknown, PhaseValidate)
}

func TestPlanModelLookupIsCaseSensitive(t *testing.T) {
	// The registry key is USM8P; a lowercase spelling is a different model
	// and must not silently match.
	_, err := BuildPlan(PlanInput{
		RunID: "run1", Network: "net", InformURL: "http://172.28.0.2:8080/inform",
		Devices: ids("usm8p"), Runtime: RuntimeConfig{}, SyntheticImage: "public/emu:1.0.0",
	})
	assertFailure(t, err, CodeSyntheticModelUnknown, PhaseValidate)
}

func TestPlanImagesAreDeduplicatedInUnitOrder(t *testing.T) {
	plan := buildPlan(t, ids("USM8P", "UXGENT", "UXGENT"), mappedConfig("UXGENT"), "public/emu:1.0.0")
	want := []string{"public/emu:1.0.0", "fixture.example/uxgent@" + testDigest}
	if got := plan.Images(); !reflect.DeepEqual(got, want) {
		t.Fatalf("images = %#v, want %#v", got, want)
	}
}

func TestOpaqueUnitCarriesItsCapabilitiesAndEndpointMAC(t *testing.T) {
	cfg := RuntimeConfig{Version: 1, Models: map[string]ModelRuntime{
		"UXGENT": {Image: "fixture.example/x@" + testDigest, CapAdd: []string{"NET_ADMIN"}},
	}}
	plan := buildPlan(t, ids("UXGENT"), cfg, "")
	unit := plan.Units[0]
	if !reflect.DeepEqual(unit.CapAdd, []string{"NET_ADMIN"}) {
		t.Fatalf("cap_add = %#v", unit.CapAdd)
	}
	if unit.EndpointMAC() != plan.Units[0].Devices[0].MAC {
		t.Fatalf("endpoint MAC = %q, want the requested MAC", unit.EndpointMAC())
	}
}

// A synthetic batch stands for several virtual devices behind one Docker
// endpoint, so it must not claim any one device's MAC on the network.
func TestSyntheticUnitRequestsNoEndpointMAC(t *testing.T) {
	plan := buildPlan(t, ids("USM8P", "UGW3"), RuntimeConfig{}, "public/emu:1.0.0")
	if got := plan.Units[0].EndpointMAC(); got != "" {
		t.Fatalf("synthetic endpoint MAC = %q, want none", got)
	}
	if len(plan.Units[0].CapAdd) != 0 {
		t.Fatalf("synthetic cap_add = %#v, want none", plan.Units[0].CapAdd)
	}
}

func TestPlanDevicesEnumeratesEveryIdentityInRequestOrder(t *testing.T) {
	plan := buildPlan(t, ids("UXGENT", "USM8P", "UXGENT"), mappedConfig("UXGENT"), "public/emu:1.0.0")
	got := plan.Devices()
	if len(got) != 3 {
		t.Fatalf("devices = %d, want 3", len(got))
	}
	for i, d := range got {
		if d.Index != i {
			t.Fatalf("device %d has index %d", i, d.Index)
		}
	}
}

func TestResolveSyntheticImagePrefersTheFlag(t *testing.T) {
	got, err := ResolveSyntheticImage("local/emu:dev", "ghcr.io/jamesbraid/unifi-emu:1.2.3")
	if err != nil {
		t.Fatalf("ResolveSyntheticImage: %v", err)
	}
	if got != "local/emu:dev" {
		t.Fatalf("image = %q, want the flag", got)
	}
}

func TestResolveSyntheticImageUsesTheCompiledDefault(t *testing.T) {
	got, err := ResolveSyntheticImage("", "ghcr.io/jamesbraid/unifi-emu:1.2.3")
	if err != nil {
		t.Fatalf("ResolveSyntheticImage: %v", err)
	}
	if got != "ghcr.io/jamesbraid/unifi-emu:1.2.3" {
		t.Fatalf("image = %q, want the compiled default", got)
	}
}

// A development build carries no compiled image default, and neither path
// may fall back to a floating tag: an unpinned "latest" would silently test
// a different emulator than the herder was built from.
func TestResolveSyntheticImageHasNoLatestFallback(t *testing.T) {
	got, err := ResolveSyntheticImage("", "")
	if err != nil {
		t.Fatalf("ResolveSyntheticImage: %v", err)
	}
	if got != "" {
		t.Fatalf("image = %q, want none so the plan can demand one", got)
	}
}

func TestDefaultSyntheticImageForVersion(t *testing.T) {
	if got := DefaultSyntheticImage("v0.4.2"); got != "ghcr.io/jamesbraid/unifi-emu:0.4.2" {
		t.Fatalf("release default = %q", got)
	}
	if got := DefaultSyntheticImage("0.4.2"); got != "ghcr.io/jamesbraid/unifi-emu:0.4.2" {
		t.Fatalf("release default without the v = %q", got)
	}
	if got := DefaultSyntheticImage("dev"); got != "" {
		t.Fatalf("development default = %q, want none", got)
	}
	if got := DefaultSyntheticImage(""); got != "" {
		t.Fatalf("empty version default = %q, want none", got)
	}
}
