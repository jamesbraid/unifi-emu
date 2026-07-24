package emu

// Profile returns the model profile for a known model. The bool is false
// for a model the generated registry does not contain. Read-only: callers
// must not mutate the returned slices.
func Profile(model string) (ModelProfile, bool) {
	p, ok := modelRegistry[model]
	return p, ok
}
