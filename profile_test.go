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

func TestModels(t *testing.T) {
	got := Models()
	if len(got) != len(modelRegistry) {
		t.Fatalf("Models() len = %d, want %d", len(got), len(modelRegistry))
	}
	// Sorted, and every name resolves through Profile.
	for i, m := range got {
		if i > 0 && got[i-1] > m {
			t.Fatalf("Models() not sorted at %d: %q > %q", i, got[i-1], m)
		}
		if _, ok := Profile(m); !ok {
			t.Fatalf("Models() returned %q, which Profile does not know", m)
		}
	}
}
