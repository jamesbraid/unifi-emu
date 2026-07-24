package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseBuildID(t *testing.T) {
	html := []byte(`<script id="__NEXT_DATA__">{"props":{},"buildId":"Min1eHzqmlYU-EcT_2cz1","other":1}</script>`)
	got, err := parseBuildID(html)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Min1eHzqmlYU-EcT_2cz1" {
		t.Fatalf("buildId = %q", got)
	}
	if _, err := parseBuildID([]byte("no build id here")); err == nil {
		t.Fatal("missing buildId: want error")
	}
}

func TestNormalizeCode(t *testing.T) {
	cases := map[string]string{
		"U7-Pro": "U7PRO", "U7 Pro": "U7PRO", "U7PRO": "U7PRO",
		"UAP-XG": "UAPXG", "a6a9": "A6A9", "": "",
	}
	for in, want := range cases {
		if got := normalizeCode(in); got != want {
			t.Errorf("normalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildFingerprintIndexMatchesCodeAndSysid(t *testing.T) {
	fp := []byte(`{"devices":[
		{"deviceTypes":["access-point"],"shortnames":["U7 Pro"],"sku":"U7 Pro","sysid":"a682","product":{"abbrev":"U7 Pro"}},
		{"deviceTypes":["access-point"],"shortnames":["U7PROXG","U7-Pro-XG"],"sku":"U7-Pro-XG","sysid":"a6a9","product":{"abbrev":"U7 Pro XG"}},
		{"deviceTypes":["switch"],"shortnames":["USW-Pro"],"sku":"USW-Pro","sysid":"beef"}
	]}`)
	idx, err := buildFingerprintIndex(fp)
	if err != nil {
		t.Fatal(err)
	}
	// friendly model code matches by name
	if sku := idx[normalizeCode("U7PRO")]; sku != "U7 Pro" {
		t.Errorf("U7PRO -> %q, want U7 Pro", sku)
	}
	// hex-coded model matches by sysid (its code normalizes to A6A9)
	if sku := idx[normalizeCode("UAPA6A9")]; sku != "" {
		t.Errorf("UAPA6A9 code should not match a name, got %q", sku)
	}
	if sku := idx[normalizeCode("a6a9")]; sku != "U7-Pro-XG" {
		t.Errorf("sysid a6a9 -> %q, want U7-Pro-XG", sku)
	}
	// switches are excluded
	if _, ok := idx[normalizeCode("USW-Pro")]; ok {
		t.Error("switch should not be indexed")
	}
}

func TestMergeEthOverridesPreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overrides.json")
	seed := `{
	  "_source_note": "note",
	  "models": {
	    "U7MP":  { "radios": { "ng": { "nss": 3 } }, "source": "hand" },
	    "USM8P": { "ports": { "plus": { "media": "GE" } }, "source": "hand" }
	  }
	}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := mergeEthOverrides(path, []ethResult{
		{model: "U7PRO", eth: ethSpec{Count: 1, Media: "2.5GbE"}, sku: "U7 Pro"},
		{model: "U7MP", eth: ethSpec{Count: 2, Media: "GE"}, sku: "UAP-AC-M-Pro"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 2 {
		t.Errorf("changed = %d, want 2", changed)
	}
	var doc struct {
		SourceNote string                   `json:"_source_note"`
		Models     map[string]modelOverride `json:"models"`
	}
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SourceNote != "note" {
		t.Errorf("_source_note dropped: %q", doc.SourceNote)
	}
	// new eth added
	if e := doc.Models["U7PRO"].Eth; e == nil || e.Media != "2.5GbE" || e.Count != 1 {
		t.Errorf("U7PRO eth = %+v", doc.Models["U7PRO"].Eth)
	}
	// existing radios preserved, eth added to the same entry
	u7mp := doc.Models["U7MP"]
	if u7mp.Radios["ng"].NSS != 3 {
		t.Errorf("U7MP radios not preserved: %+v", u7mp.Radios)
	}
	if u7mp.Eth == nil || u7mp.Eth.Count != 2 {
		t.Errorf("U7MP eth not added: %+v", u7mp.Eth)
	}
	if u7mp.Source != "hand" {
		t.Errorf("U7MP existing source overwritten: %q", u7mp.Source)
	}
	// existing ports-only entry untouched
	if doc.Models["USM8P"].Ports["plus"].Media != "GE" {
		t.Errorf("USM8P ports not preserved")
	}
}
