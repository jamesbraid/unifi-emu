package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// capsFile is the capability vocabulary scripts/extract_caps.py reads out
// of the controller's Java device model. Only the bit names and values
// matter here; the provenance fields are for whoever reads the file.
type capsFile struct {
	ControllerVersion string               `json:"controller_version"`
	Bitmaps           map[string]capBitmap `json:"bitmaps"`
}

// capBitmap is one bitmap: the device-document field it is read from and
// the named bits it holds.
type capBitmap struct {
	Field string         `json:"field"`
	Bits  map[string]int `json:"bits"`
}

// udapiFamily is the constant family in capability_bits.json holding the
// bits of the udapi_caps bitmap.
const udapiFamily = "UNIFI_UDAPI_CAP"

func loadCaps(path string) (capsFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return capsFile{}, err
	}
	var c capsFile
	if err := json.Unmarshal(b, &c); err != nil {
		return capsFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(c.Bitmaps[udapiFamily].Bits) == 0 {
		return capsFile{}, fmt.Errorf("%s: no %s bits; re-run scripts/extract_caps.py", path, udapiFamily)
	}
	return c, nil
}

// udapiMask ORs named UDAPI capability bits into the bitmap a device
// reports. An unknown name is fatal rather than skipped: a typo that
// silently produced a zero mask would reproduce the bug this whole
// mechanism exists to fix, and quietly claim nothing.
func (c capsFile) udapiMask(model string, names []string) (int, error) {
	bits := c.Bitmaps[udapiFamily].Bits
	mask := 0
	for _, name := range names {
		value, ok := bits[name]
		if !ok {
			return 0, fmt.Errorf("%s: unknown UDAPI capability %q; known bits are %s",
				model, name, strings.Join(sortedKeys(bits), ", "))
		}
		mask |= value
	}
	return mask, nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
