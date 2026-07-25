package main

import "sort"

// excludedModels are model codes the hardware DB carries but that no
// controller will adopt, mapped to the evidence for dropping each one. The
// catalog is the set of models the emulator can pretend to be, so a code that
// never reaches a pending document does not belong in it: it costs any fleet
// that draws it a full adoption timeout and reports nothing about why.
//
// A hand-maintained table rather than a rule derived from the bundle is
// deliberate. The bundle's own adoptability field reads "adoptable" for every
// model here, and no harvested field separates them from models that work, so
// the only honest basis for an exclusion is a measurement, recorded per model.
// Promote this to a rule if a field ever turns out to predict the behaviour.
var excludedModels = map[string]string{
	"UGWHD4": "no hardware reports this code. Ubiquiti's fingerprint DB files it " +
		"as an alias on the USG product record with an empty sysids list, while " +
		"real USG hardware identifies as UGW3 (sysid ee21); it has no firmware " +
		"bundle and no friendly name. Measured against sim controller 10.4.57: it " +
		"informs and is answered HTTP 404 like any pending device, but the " +
		"controller never creates a pending document for it -- alone or beside a " +
		"device that does appear -- and logs nothing about it, while UGW3 and " +
		"UXGENT adopt to CONNECTED from the same payload branch",
}

// excludedModel records an exclusion in the generated catalog, so the artifact
// carries its own explanation instead of leaving the next reader to find the
// reasoning in git log.
type excludedModel struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// excludedCatalogList returns the exclusions in a stable order.
func excludedCatalogList() []excludedModel {
	out := make([]excludedModel, 0, len(excludedModels))
	for model, reason := range excludedModels {
		out = append(out, excludedModel{Model: model, Reason: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// staleExclusions returns the excluded models absent from the bundle. Such an
// entry has outlived its subject: it keeps asserting a measurement about a
// model this bundle does not offer. That is worth reporting, but not worth
// failing a run over -- a partial or older bundle legitimately lacks models,
// and refusing to generate would make every exclusion a hard dependency on
// the bundle it was measured against.
func staleExclusions(considered map[string]bool) []string {
	var stale []string
	for model := range excludedModels {
		if !considered[model] {
			stale = append(stale, model)
		}
	}
	sort.Strings(stale)
	return stale
}
