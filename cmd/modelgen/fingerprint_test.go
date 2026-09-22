package main

import (
	"encoding/json"
	"testing"
)

// A model's outlet categories are not disjoint: some models list the same
// index under more than one category, so the outlet set is their union. Summing
// the categories reports a device with twice the outlets it has, which is the
// failure this guards.
func TestDeriveOutletsUnionsOverlappingCategories(t *testing.T) {
	meta := deviceDBModel{Outlets: map[string]json.RawMessage{
		"standard": json.RawMessage(`[1,2,3,4]`),
		"surge":    json.RawMessage(`[1,2,3,4]`),
	}}
	outlets, err := deriveOutlets(meta)
	if err != nil {
		t.Fatalf("deriveOutlets: %v", err)
	}
	if len(outlets) != 4 {
		t.Fatalf("got %d outlets, want 4 -- overlapping categories were summed", len(outlets))
	}
	for i, o := range outlets {
		if o.Index != i+1 {
			t.Errorf("outlet %d has index %d, want %d", i, o.Index, i+1)
		}
	}
}

// Where the categories are disjoint, each index keeps its own group: the group
// is what tells a surge-only outlet from one on the battery bank.
func TestDeriveOutletsKeepsDisjointGroups(t *testing.T) {
	meta := deviceDBModel{Outlets: map[string]json.RawMessage{
		"standard": json.RawMessage(`[1,2]`),
		"surge":    json.RawMessage(`[3,4]`),
		"usb":      json.RawMessage(`"5-6"`),
	}}
	outlets, err := deriveOutlets(meta)
	if err != nil {
		t.Fatalf("deriveOutlets: %v", err)
	}
	want := map[int]string{1: "standard", 2: "standard", 3: "surge", 4: "surge", 5: "usb", 6: "usb"}
	if len(outlets) != len(want) {
		t.Fatalf("got %d outlets, want %d", len(outlets), len(want))
	}
	for _, o := range outlets {
		if o.Group != want[o.Index] {
			t.Errorf("outlet %d group = %q, want %q", o.Index, o.Group, want[o.Index])
		}
	}
}

// The jack categories share the outlet map but are not outlets: a model that
// lists its network sockets there would otherwise grow phantom outlets the
// controller would render as switchable power.
func TestDeriveOutletsSkipsNetworkJacks(t *testing.T) {
	meta := deviceDBModel{Outlets: map[string]json.RawMessage{
		"standard": json.RawMessage(`[1,2]`),
		"lan":      json.RawMessage(`[3]`),
		"rj45":     json.RawMessage(`[4,5]`),
		"wan":      json.RawMessage(`[6]`),
	}}
	outlets, err := deriveOutlets(meta)
	if err != nil {
		t.Fatalf("deriveOutlets: %v", err)
	}
	if len(outlets) != 2 {
		t.Fatalf("got %d outlets, want 2 -- network jacks were counted as outlets", len(outlets))
	}
}

// A model with no outlet map is the common case and must stay empty rather than
// growing a zero-length table: an outlet table is itself the signal that a
// device is a power device.
func TestDeriveOutletsEmptyWithoutAnOutletMap(t *testing.T) {
	outlets, err := deriveOutlets(deviceDBModel{})
	if err != nil {
		t.Fatalf("deriveOutlets: %v", err)
	}
	if outlets != nil {
		t.Errorf("got %v, want no outlets", outlets)
	}
}

const fingerprintSample = `{"devices":[
  {"product":{"name":"Power Distribution Pro"},"unifi":{"network":{
     "model":"USPPDUP","type":"usw","ports":{"standard":1},
     "outlets":{"usb":"1-4","standard":"5-8"}}}},
  {"product":{"name":"Gateway Enterprise"},"unifi":{"network":{
     "model":"UXGENT","type":"uxg","ports":{"eth0":"WAN","eth1":"LAN"}}}},
  {"product":{"name":"Something Else"},"unifi":{}},
  {"product":{"name":"No Model Code"},"unifi":{"network":{"type":"usw"}}}
]}`

func TestFingerprintModelsReadsNetworkDevices(t *testing.T) {
	models, display, err := fingerprintModels([]byte(fingerprintSample))
	if err != nil {
		t.Fatalf("fingerprintModels: %v", err)
	}
	// Entries without a network model code are not network devices and are
	// skipped rather than landing in the catalog under an empty key.
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2: %v", len(models), models)
	}
	if models["USPPDUP"].Type != "usw" {
		t.Errorf("USPPDUP type = %q, want usw", models["USPPDUP"].Type)
	}
	if len(models["USPPDUP"].Outlets) == 0 {
		t.Errorf("USPPDUP carries no outlet map")
	}
	// The published product name is what gives the many code-named models a
	// real display name.
	if display["UXGENT"] != "Gateway Enterprise" {
		t.Errorf("UXGENT display = %q, want %q", display["UXGENT"], "Gateway Enterprise")
	}
}

func TestFingerprintModelsRejectsAnEmptyDatabase(t *testing.T) {
	if _, _, err := fingerprintModels([]byte(`{"devices":[]}`)); err == nil {
		t.Error("an empty database was accepted; a harvest from it would silently produce nothing")
	}
}

// Two databases describing the same model differently is the condition the
// cross-check exists to catch, so each kind of disagreement has to surface
// rather than one source quietly winning.
func TestCompareCatalogsReportsDisagreements(t *testing.T) {
	a := catalogFile{Models: []catalogModel{
		{Model: "A", Type: "usw", Ports: []catalogPort{{PortIdx: 1, Media: "GE"}}},
		{Model: "B", Type: "usw", Ports: []catalogPort{{PortIdx: 1, Media: "GE"}}},
		{Model: "OnlyA", Type: "usw"},
	}}
	b := catalogFile{Models: []catalogModel{
		{Model: "A", Type: "usw", Ports: []catalogPort{{PortIdx: 1, Media: "SFP28"}}},
		{Model: "B", Type: "usw", Ports: []catalogPort{{PortIdx: 1, Media: "GE"}}},
		{Model: "OnlyB", Type: "usw"},
	}}
	conflicts, onlyA, onlyB := compareCatalogs(a, b, "left", "right")

	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: %v", len(conflicts), conflicts)
	}
	if len(onlyA) != 1 || onlyA[0] != "OnlyA" {
		t.Errorf("onlyA = %v, want [OnlyA]", onlyA)
	}
	if len(onlyB) != 1 || onlyB[0] != "OnlyB" {
		t.Errorf("onlyB = %v, want [OnlyB]", onlyB)
	}
}

// A name the primary source supplies is the curated one and must not be
// replaced by the public database's wording.
func TestMergeDisplayPrefersThePrimarySource(t *testing.T) {
	got := mergeDisplay(
		map[string]string{"A": "Curated", "B": ""},
		map[string]string{"A": "Published", "B": "Published", "C": "Published"},
	)
	want := map[string]string{"A": "Curated", "B": "Published", "C": "Published"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// fw_caps is a property of a firmware branch for almost every model, so the
// type-level entry is what a model normally gets. A model measured to differ
// from its branch-mates -- a switch on a different chipset reports a different
// switch bit than the model beside it on the same build -- says so with its own
// entry, and that has to win rather than being averaged away by the branch.
func TestFirmwareCapsPrefersTheModelEntry(t *testing.T) {
	ov := overrides{FirmwareCaps: map[string]firmwareCapsOverride{
		"usw@7.4.1":     {FWCaps: 111},
		"USWED74@7.4.1": {FWCaps: 222},
	}}
	cases := []struct {
		name  string
		model string
		typ   string
		want  int
	}{
		{"a model with its own entry takes it", "USWED74", "usw", 222},
		{"a model without one takes the branch's", "USWED76", "usw", 111},
		// Neither key matching leaves the model on the placeholder rather
		// than borrowing a bitmap nobody measured on it.
		{"a model on an uncaptured branch gets nothing", "USWED76", "uap", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := catalogModel{Model: tc.model, Type: tc.typ, Version: "7.4.1"}
			if fc, ok := ov.FirmwareCaps[m.Model+"@"+m.Version]; ok {
				m.FWCaps = fc.FWCaps
			} else if fc, ok := ov.FirmwareCaps[m.Type+"@"+m.Version]; ok {
				m.FWCaps = fc.FWCaps
			}
			if m.FWCaps != tc.want {
				t.Errorf("fw_caps = %d, want %d", m.FWCaps, tc.want)
			}
		})
	}
}

// A switch commonly powers only some of its ports, and a PoE-powered switch
// has an uplink that takes power in. Describing either as delivering power
// offers a capability the device will not honour, so the ports that do have to
// be nameable.
func TestPoEPortsRestrictsWhichPortsDeliverPower(t *testing.T) {
	meta := deviceDBModel{
		Ports: map[string]json.RawMessage{"standard": json.RawMessage(`8`)},
		Features: struct {
			PoE bool `json:"poe"`
		}{PoE: true},
	}

	all, err := switchMetadataPorts(meta, modelOverride{})
	if err != nil {
		t.Fatalf("switchMetadataPorts: %v", err)
	}
	powered := func(ports []catalogPort) []int {
		var out []int
		for _, p := range ports {
			if p.PoECaps != 0 {
				out = append(out, p.PortIdx)
			}
		}
		return out
	}
	// Absent stays the existing behaviour: every copper port is powered.
	if got := len(powered(all)); got != 8 {
		t.Errorf("with no override %d of 8 ports are powered, want all 8", got)
	}

	some, err := switchMetadataPorts(meta, modelOverride{PoEPorts: "1-4"})
	if err != nil {
		t.Fatalf("switchMetadataPorts: %v", err)
	}
	want := []int{1, 2, 3, 4}
	if got := powered(some); len(got) != len(want) {
		t.Errorf("powered ports = %v, want %v", got, want)
	}
	// The unpowered ports keep their media; only the power claim goes.
	if some[7].Media != "GE" {
		t.Errorf("port 8 media = %q, want it untouched", some[7].Media)
	}

	// An unparseable list fails the model rather than quietly powering
	// everything, which is the failure that would go unnoticed.
	if _, err := switchMetadataPorts(meta, modelOverride{PoEPorts: "not-a-range"}); err == nil {
		t.Error("an unparseable poe_ports was accepted")
	}
}

// A captured hw_caps replaces the derived one, except that the outlet bit
// the derivation set stays set: an outlet model that lost it would adopt,
// report its outlets on every inform, and show none of them.
func TestHWCapsOverrideKeepsTheOutletBit(t *testing.T) {
	lcm, poePlus := 8, 4096
	m := catalogModel{HWCaps: hwCapOutlet}
	applyOverride(&m, modelOverride{HWCaps: &lcm})
	if m.HWCaps != lcm|hwCapOutlet {
		t.Errorf("outlet model with a captured LCM bit = %d, want %d (LCM plus the outlet bit)", m.HWCaps, lcm|hwCapOutlet)
	}
	m = catalogModel{}
	applyOverride(&m, modelOverride{HWCaps: &poePlus})
	if m.HWCaps != poePlus {
		t.Errorf("non-outlet model = %d, want the captured %d untouched", m.HWCaps, poePlus)
	}
	m = catalogModel{HWCaps: hwCapOutlet}
	applyOverride(&m, modelOverride{})
	if m.HWCaps != hwCapOutlet {
		t.Errorf("no override changed hw_caps to %d", m.HWCaps)
	}
}
