package firmware

import (
	"strings"
	"testing"
)

func TestParseLiveEvidenceAndCompare(t *testing.T) {
	live, err := ParseLiveEvidence(strings.NewReader(`{
		"cfgversion":"abc",
		"fw_caps":3,
		"nested":{"cmd":"setparam","poe_caps":7}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	foundMask := false
	for _, observation := range live {
		if observation.Value == "fw_caps" && observation.ObservedValue == "3" {
			foundMask = true
		}
	}
	if !foundMask {
		t.Fatalf("live bitmap value was lost: %+v", live)
	}
	static := []Observation{
		{Kind: "capability", Value: "fw_caps", Artifact: "mcad"},
		{Kind: "command", Value: "upgrade", Artifact: "mcad"},
	}
	comparison := CompareEvidence(static, live)
	if strings.Join(comparison.Agreed, ",") != "capability:fw_caps" {
		t.Fatalf("agreed = %+v", comparison.Agreed)
	}
	if strings.Join(comparison.StaticOnly, ",") != "command:upgrade" {
		t.Fatalf("static only = %+v", comparison.StaticOnly)
	}
	for _, want := range []string{"capability:poe_caps", "command:setparam", "inform_field:cfgversion"} {
		found := false
		for _, value := range comparison.LiveOnly {
			found = found || value == want
		}
		if !found {
			t.Errorf("live only missing %q: %+v", want, comparison.LiveOnly)
		}
	}
}

func TestParseLiveEvidenceAcceptsDumpText(t *testing.T) {
	live, err := ParseLiveEvidence(strings.NewReader("fw_caps=3\ncmd=setstate\ncfgversion=abc\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 3 {
		t.Fatalf("live observations = %+v", live)
	}
	if live[0].ObservedValue == "" && live[1].ObservedValue == "" && live[2].ObservedValue == "" {
		t.Fatalf("dump values were lost: %+v", live)
	}
}

func TestParseLiveEvidenceRejectsOversizedInput(t *testing.T) {
	if _, err := ParseLiveEvidence(strings.NewReader(strings.Repeat("x", (16<<20)+1))); err == nil {
		t.Fatal("accepted live evidence over 16 MiB")
	}
}

func TestParseLiveEvidencePreservesExactBitmapIntegers(t *testing.T) {
	live, err := ParseLiveEvidence(strings.NewReader(`{"feature_caps":18446744073709551615}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ObservedValue != "18446744073709551615" {
		t.Fatalf("large bitmap value was rounded: %+v", live)
	}
}

func TestParseLiveSnapshotPreservesCompleteTypedJSONTree(t *testing.T) {
	_, fields, err := ParseLiveSnapshot(strings.NewReader(
		`{"name":"U7PRO","feature_caps":18446744073709551615,"radios":[{"enabled":true}],"missing":null}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]LiveField{
		"":                  {Path: "", Type: "object"},
		"/name":             {Path: "/name", Type: "string", Value: "U7PRO"},
		"/feature_caps":     {Path: "/feature_caps", Type: "number", Value: "18446744073709551615"},
		"/radios":           {Path: "/radios", Type: "array"},
		"/radios/0":         {Path: "/radios/0", Type: "object"},
		"/radios/0/enabled": {Path: "/radios/0/enabled", Type: "boolean", Value: "true"},
		"/missing":          {Path: "/missing", Type: "null", Value: "null"},
	}
	for _, field := range fields {
		delete(want, field.Path)
	}
	if len(want) != 0 {
		t.Fatalf("missing typed fields: %+v; got %+v", want, fields)
	}
}
