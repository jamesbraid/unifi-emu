package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type overrides struct {
	Models map[string]modelOverride `json:"models"`
	// FirmwareCaps is keyed "<type>@<version>", because fw_caps is mostly
	// a property of a firmware branch rather than of a SKU: three switch
	// models on 7.4.1.16850 report byte-identical bitmaps, and within one
	// AP firmware the whole model-to-model spread is a bit or two. Keying
	// it per model throughout would imply a precision the captures do not
	// have, and would leave 96% of the catalog guessing.
	//
	// A "<model>@<version>" entry is also accepted, and wins where it
	// exists. Mostly the branch is the right unit, but not always: models
	// on one firmware can differ where the hardware does, and a switch
	// built on a different chipset reports a different switch bit than its
	// branch-mate. The per-model key is for a model measured to differ,
	// not for filling in models nobody has captured.
	//
	// Neither key matching gets nothing, so a model on firmware nobody has
	// captured keeps the placeholder rather than borrowing a neighbour's
	// bitmap.
	FirmwareCaps map[string]firmwareCapsOverride `json:"firmware_caps,omitempty"`
}

type firmwareCapsOverride struct {
	FWCaps int    `json:"fw_caps"`
	Source string `json:"source,omitempty"`
}

type modelOverride struct {
	Display string                   `json:"display,omitempty"`
	Eth     *ethOverride             `json:"eth,omitempty"`
	Radios  map[string]radioOverride `json:"radios,omitempty"`
	Ports   map[string]portOverride  `json:"ports,omitempty"`
	// PoE restates whether the switch has a PSE at all. It is a pointer
	// because the interesting case is false: the bundle marks a couple of
	// non-PoE SKUs PoE-capable by inheriting their PoE sibling's record,
	// and an absent key has to stay distinguishable from a deliberate no.
	PoE *bool `json:"poe,omitempty"`
	// PoEPorts names the ports that deliver power, in the bundle's own
	// "1-16,19" notation, for a switch that powers only some of them. The
	// derivation otherwise flags every copper port, which is right for the
	// Pro and Enterprise lines and wrong for most of the rest: a switch
	// commonly powers half its ports, and a PoE-powered switch has an
	// uplink that takes power *in* and must not be described as giving it
	// out.
	//
	// Absent means every copper port delivers power, which stays the
	// common case and the safe default: a port wrongly described as
	// powered offers capability the device will not honour, while one
	// wrongly described as unpowered only understates it.
	PoEPorts string         `json:"poe_ports,omitempty"`
	UDAPI    *udapiOverride `json:"udapi,omitempty"`
	// HWCaps restates the hardware bitmap as a real unit of the model
	// reports it, for the models somebody has captured. The derivation
	// otherwise knows only the outlet bit, so a screen, an RPS port or the
	// PoE class an AP takes go unclaimed. A pointer, because a captured
	// zero is a fact worth stating and an absent key is not one.
	HWCaps *int   `json:"hw_caps,omitempty"`
	Source string `json:"source,omitempty"`
}

// udapiOverride is what a model reports for the UDAPI config plane.
// Which models really have which capability is a fact about hardware
// that exists nowhere in the controller -- it learns the bitmap from the
// device -- so it is curated here from Ubiquiti's published support
// matrix, with the citation in the entry's source.
//
// Caps names bits from capability_bits.json rather than a number, so
// the entry says what it claims and a typo fails the build.
type udapiOverride struct {
	Version string   `json:"version"`
	Caps    []string `json:"caps"`
}

type ethOverride struct {
	Count int    `json:"count"`
	Media string `json:"media"`
}
type radioOverride struct {
	NSS int `json:"nss,omitempty"`
}

// portOverride restates one of the bundle's port categories. Media replaces
// the connector the category implies; Indexes replaces the set of port numbers
// it covers, in the bundle's own "1-24,26" notation, and may name a category
// the bundle omits entirely so a miscategorised port can be moved rather than
// only relabelled.
type portOverride struct {
	Media   string `json:"media,omitempty"`
	Indexes string `json:"indexes,omitempty"`
}

func loadOverrides(path string) (overrides, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return overrides{}, err
	}
	var ov overrides
	if err := json.Unmarshal(b, &ov); err != nil {
		return overrides{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return ov, nil
}

// applyOverride patches a derived catalog model. Eth builds the AP port
// layout the bundle can't supply; Radios patch nss by band. Display, Ports
// and PoE are consumed during derivation instead, because they decide what
// gets derived rather than adjusting the result.
func applyOverride(m *catalogModel, o modelOverride) {
	if o.Eth != nil {
		count := o.Eth.Count
		if count <= 0 {
			count = 1
		}
		m.Ports = m.Ports[:0]
		for i := 1; i <= count; i++ {
			m.Ports = append(m.Ports, catalogPort{
				IfName: fmt.Sprintf("eth%d", i-1), Name: fmt.Sprintf("eth%d", i-1),
				PortIdx: i, Media: o.Eth.Media, IsUplink: i == 1,
			})
		}
	}
	// The outlet bit is kept whatever the override says: a model with
	// outlets needs it for the controller to keep its outlet table, and a
	// capture that includes it agrees anyway.
	if o.HWCaps != nil {
		m.HWCaps = *o.HWCaps | (m.HWCaps & hwCapOutlet)
	}
	for band, ro := range o.Radios {
		for i := range m.Radios {
			if m.Radios[i].Radio == band && ro.NSS > 0 {
				m.Radios[i].NSS = ro.NSS
			}
		}
	}
}

func checkStaleOverrides(ov overrides, models map[string]bool) error {
	for model := range ov.Models {
		if !models[model] {
			return fmt.Errorf("stale override: model %q not in the generated catalog", model)
		}
	}
	return nil
}
