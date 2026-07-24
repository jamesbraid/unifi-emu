package emu_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	emu "github.com/jamesbraid/unifi-emu"
)

func TestEvidenceDirUsesSafeTestName(t *testing.T) {
	got := evidenceDir("TestClassic/Fleet with spaces")
	want := filepath.Join("tmp", "itest", "testclassic_fleet_with_spaces")
	if got != want {
		t.Fatalf("evidenceDir() = %q, want %q", got, want)
	}
}

func TestWriteJSONPreservesDeviceDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending-devices.json")
	devices := []emu.Device{{
		MAC:     "00:27:22:e0:00:01",
		State:   2,
		Adopted: false,
		Model:   "UGW3",
	}}
	if err := writeJSON(path, devices); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"mac": "00:27:22:e0:00:01"`,
		`"state": 2`,
		`"adopted": false`,
		`"model": "UGW3"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("%s does not contain %q:\n%s", path, want, body)
		}
	}
}
