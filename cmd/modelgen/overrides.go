package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type overrides struct {
	Models map[string]modelOverride `json:"models"`
}

type modelOverride struct {
	Eth    *ethOverride             `json:"eth,omitempty"`
	Radios map[string]radioOverride `json:"radios,omitempty"`
	Ports  map[string]portOverride  `json:"ports,omitempty"`
	Source string                   `json:"source,omitempty"`
}

type ethOverride struct {
	Count int    `json:"count"`
	Media string `json:"media"`
}
type radioOverride struct {
	NSS int `json:"nss,omitempty"`
}
type portOverride struct {
	Media string `json:"media,omitempty"`
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
// layout the bundle can't supply; Radios patch nss by band; Ports patch a
// port's media by the bundle port category name (handled in derivation).
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
