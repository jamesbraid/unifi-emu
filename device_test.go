package emu

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeviceSpecScalar(t *testing.T) {
	var s DeviceSpec
	if err := yaml.Unmarshal([]byte(`U7PRO`), &s); err != nil {
		t.Fatalf("scalar: %v", err)
	}
	if s.Model != "U7PRO" {
		t.Fatalf("Model = %q, want U7PRO", s.Model)
	}
}

func TestDeviceSpecMapping(t *testing.T) {
	var s DeviceSpec
	in := `{model: UGW3, ip: 10.0.0.1, mac: "00:11:22:33:44:55"}`
	if err := yaml.Unmarshal([]byte(in), &s); err != nil {
		t.Fatalf("mapping: %v", err)
	}
	if s.Model != "UGW3" || s.IP != "10.0.0.1" || s.MAC != "00:11:22:33:44:55" {
		t.Fatalf("got %+v", s)
	}
}

func TestDeviceSpecSerial(t *testing.T) {
	// serial is part of the fleet-file contract, so the strict key check
	// must accept it: a device container is told its serial, it does not
	// derive one.
	var s DeviceSpec
	if err := yaml.Unmarshal([]byte(`{model: UGW3, serial: F09FC2ABCDEF}`), &s); err != nil {
		t.Fatalf("mapping: %v", err)
	}
	if s.Serial != "F09FC2ABCDEF" {
		t.Fatalf("Serial = %q, want F09FC2ABCDEF", s.Serial)
	}
}

func TestDeviceSpecUnknownKeyRejected(t *testing.T) {
	var s DeviceSpec
	// "modle" is a misspelling of "model"; must fail, not silently drop.
	err := yaml.Unmarshal([]byte(`{modle: UGW3}`), &s)
	if err == nil {
		t.Fatal("unknown key accepted, want error")
	}
}

func TestDeviceSpecList(t *testing.T) {
	var list []DeviceSpec
	in := `["U7PRO", {model: UGW3, ip: 10.0.0.2}]`
	if err := yaml.Unmarshal([]byte(in), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Model != "U7PRO" || list[1].Model != "UGW3" {
		t.Fatalf("got %+v", list)
	}
}
