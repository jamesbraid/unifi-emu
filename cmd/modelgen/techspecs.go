package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// ethSpec is an access point's Ethernet uplink layout, sourced from Tech
// Specs because the hardware DB bundle omits AP port data entirely.
type ethSpec struct {
	Count int
	Media string
}

var ethCountRe = regexp.MustCompile(`\((\d+)\)`)

// parseEthValue parses a networking-interface string like
// "(1) 1/2.5 GbE RJ45 port" into a count and normalized media.
func parseEthValue(s string) (ethSpec, bool) {
	m := ethCountRe.FindStringSubmatch(s)
	count := 1
	if m != nil {
		count, _ = strconv.Atoi(m[1])
	}
	media, ok := normalizeMedia(s)
	if !ok || count <= 0 {
		return ethSpec{}, false
	}
	return ethSpec{Count: count, Media: media}, true
}

// normalizeMedia resolves the physical connector/media type out of a raw
// networking-interface string. Priority matters: strings like "10G SFP+"
// contain both a speed token and a connector token, and the connector is
// the more specific, user-meaningful fact (SFP+ vs plain 10GbE copper), so
// connector tokens are checked before bare speed tokens.
func normalizeMedia(s string) (string, bool) {
	u := strings.ToUpper(s)
	switch {
	case strings.Contains(u, "SFP28"):
		return "SFP28", true
	case strings.Contains(u, "SFP+"), strings.Contains(u, "SFP "):
		return "SFP+", true
	case strings.Contains(u, "2.5") && strings.Contains(u, "GBE"):
		return "2.5GbE", true
	case strings.Contains(u, "10G"), strings.Contains(u, "10 G"):
		return "10GbE", true
	case strings.Contains(u, "GBE"):
		return "GE", true
	}
	return "", false
}

// parseNetworkingInterface pulls the "networking-interface" spec value out of
// a techspecs _next/data product JSON and parses it.
func parseNetworkingInterface(data []byte) (ethSpec, bool) {
	// The spec entries are nested; decode loosely and walk for the feature
	// slug "networking-interface".
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return ethSpec{}, false
	}
	value, ok := findNetworkingInterface(doc)
	if !ok {
		return ethSpec{}, false
	}
	return parseEthValue(value)
}

// findNetworkingInterface walks the decoded JSON for an object shaped like
// {"value": "...", "feature": {"slug": "networking-interface"}}.
func findNetworkingInterface(node any) (string, bool) {
	switch n := node.(type) {
	case map[string]any:
		if f, ok := n["feature"].(map[string]any); ok {
			if slug, _ := f["slug"].(string); slug == "networking-interface" {
				if v, ok := n["value"].(string); ok {
					return v, true
				}
			}
		}
		for _, v := range n {
			if s, ok := findNetworkingInterface(v); ok {
				return s, true
			}
		}
	case []any:
		for _, v := range n {
			if s, ok := findNetworkingInterface(v); ok {
				return s, true
			}
		}
	}
	return "", false
}

// modelSlug derives a techspecs URL slug from a fingerprint SKU or product name,
// e.g. "U7 Pro" -> "u7-pro". Unresolved slugs 404 and the AP falls back to
// the GE default (logged) in the refresh step.
func modelSlug(productName string) string {
	s := strings.ToLower(strings.TrimSpace(productName))
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.Join(strings.Fields(s), "-")
	return s
}
