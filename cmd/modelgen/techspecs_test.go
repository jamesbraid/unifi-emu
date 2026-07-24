package main

import (
	"os"
	"testing"
)

func TestParseEthValue(t *testing.T) {
	cases := map[string]ethSpec{
		"(1) 1/2.5 GbE RJ45 port": {Count: 1, Media: "2.5GbE"},
		"(1) GbE RJ45 port":       {Count: 1, Media: "GE"},
		"(2) GbE RJ45 ports":      {Count: 2, Media: "GE"},
		"(1) 10G SFP+ port":       {Count: 1, Media: "SFP+"},
	}
	for in, want := range cases {
		got, ok := parseEthValue(in)
		if !ok || got != want {
			t.Errorf("parseEthValue(%q) = %+v ok=%v, want %+v", in, got, ok, want)
		}
	}
}

func TestParseNetworkingInterface(t *testing.T) {
	data, err := os.ReadFile("testdata/techspecs-u7pro.json")
	if err != nil {
		t.Fatal(err)
	}
	eth, ok := parseNetworkingInterface(data)
	if !ok {
		t.Fatal("no networking-interface found")
	}
	if eth.Media != "2.5GbE" || eth.Count != 1 {
		t.Fatalf("U7 Pro eth = %+v, want {1 2.5GbE}", eth)
	}
}

func TestModelSlug(t *testing.T) {
	if got := modelSlug("U7 Pro"); got != "u7-pro" {
		t.Fatalf("modelSlug = %q, want u7-pro", got)
	}
}
