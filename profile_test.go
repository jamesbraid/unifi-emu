package emu

import "testing"

func TestProfileKnown(t *testing.T) {
	p, ok := Profile("UGW3")
	if !ok {
		t.Fatal("Profile(UGW3) ok=false, want true")
	}
	if p.Type != "ugw" {
		t.Fatalf("Profile(UGW3).Type = %q, want ugw", p.Type)
	}
}

func TestProfileUnknown(t *testing.T) {
	if _, ok := Profile("NOPE"); ok {
		t.Fatal("Profile(NOPE) ok=true, want false")
	}
}
