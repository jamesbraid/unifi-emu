package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestParseFirmware(t *testing.T) {
	b, err := os.ReadFile("testdata/firmware-latest.json")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := parseFirmware(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// platform==model join; version transform v4.4.57+5578372 -> 4.4.57.5578372
	if got := idx["UGW3"]; got == "" || got[0] == 'v' || strings.Contains(got, "+") {
		t.Fatalf("UGW3 version = %q, want transformed X.Y.Z.build", got)
	}
}

func TestFirmwareVersionFallback(t *testing.T) {
	idx := firmwareIndex{"UGW3": "4.4.57.5578372"}
	if got := firmwareVersion(idx, "UGW3", "ugw"); got != "4.4.57.5578372" {
		t.Fatalf("matched = %q", got)
	}
	// Unmatched AP falls back to the per-type default (non-empty).
	if got := firmwareVersion(idx, "UNMATCHED", "uap"); got == "" {
		t.Fatal("unmatched uap version empty, want per-type default")
	}
	// Every emulated type needs a default: an empty version fails
	// validateModel, so a type without one silently drops any model the
	// firmware API does not list.
	for _, typ := range []string{"uap", "usw", "ugw", "uxg"} {
		if got := firmwareVersion(idx, "UNMATCHED", typ); got == "" {
			t.Errorf("unmatched %s version empty, want a per-type default", typ)
		}
	}
}
