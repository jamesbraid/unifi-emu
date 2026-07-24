package emu

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// modelCatalogJSON is the reduced model catalog, embedded at build time.
// It's derived (manually, via cmd/modelgen) from the controller's stat/device
// identity dump plus the hardware database in its UI bundle; that source
// bundle is ~5MB and not checked in, so model_profiles.json itself stays
// committed as the source of truth and is embedded here rather than
// regenerated at build time.
//
//go:embed model_profiles.json
var modelCatalogJSON []byte

// modelCatalog mirrors cmd/modelgen's catalogFile shape closely enough to
// unmarshal model_profiles.json directly into ModelProfile values.
type modelCatalog struct {
	Models []ModelProfile `json:"models"`
}

// defaultChannel returns the channel a radio band defaults to when the
// catalog doesn't carry one explicitly. model_profiles.json omits channel
// (it's not part of a model's static hardware facts), so the runtime loader
// derives it the same way cmd/modelgen's identically named helper did when
// it used to bake the registry into Go source.
func defaultChannel(radio string) int {
	switch radio {
	case "ng":
		return 1
	case "na":
		return 36
	default:
		return 5
	}
}

// loadRegistry parses the embedded model catalog into the runtime registry.
// A parse failure or an empty catalog means the embedded asset is broken or
// was never populated, which is a build/data bug, not a runtime condition
// callers can recover from — so it panics loudly at startup rather than
// leaving modelRegistry silently empty.
func loadRegistry() map[string]ModelProfile {
	var catalog modelCatalog
	if err := json.Unmarshal(modelCatalogJSON, &catalog); err != nil {
		panic(fmt.Sprintf("emu: failed to parse embedded model_profiles.json: %v", err))
	}
	if len(catalog.Models) == 0 {
		panic("emu: embedded model_profiles.json contains no models")
	}
	registry := make(map[string]ModelProfile, len(catalog.Models))
	for _, m := range catalog.Models {
		for i := range m.Radios {
			m.Radios[i].Channel = defaultChannel(m.Radios[i].Radio)
		}
		registry[m.Model] = m
	}
	return registry
}
