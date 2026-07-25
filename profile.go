package emu

import "sort"

// Profile returns the model profile for a known model. The bool is false
// for a model the generated registry does not contain. Read-only: callers
// must not mutate the returned slices.
func Profile(model string) (ModelProfile, bool) {
	p, ok := modelRegistry[model]
	return p, ok
}

// Models returns the names of every model in the generated registry, sorted
// for a stable order. Read-only view for callers that enumerate the known
// models, such as a UI listing what the emulator can pretend to be.
func Models() []string {
	names := make([]string, 0, len(modelRegistry))
	for m := range modelRegistry {
		names = append(names, m)
	}
	sort.Strings(names)
	return names
}
