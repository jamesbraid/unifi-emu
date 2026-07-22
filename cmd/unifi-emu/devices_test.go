package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jamesbraid/unifi-emu"
)

// The compose-sidecar contract (terraform-provider-unifi): SIM_DEVICES is
// an inline YAML list of DeviceSpec.
const composeFleet = `
- {mac: "00:27:22:00:00:02", type: usw, model: USWED74}
- {mac: "00:27:22:e0:00:51", type: uap, model: U7MP}
`

var composeSpecs = []emu.DeviceSpec{
	{MAC: "00:27:22:00:00:02", Type: "usw", Model: "USWED74"},
	{MAC: "00:27:22:e0:00:51", Type: "uap", Model: "U7MP"},
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "devices.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDevicesYAMLFile(t *testing.T) {
	// Block-style YAML exercising every DeviceSpec key.
	f := writeTemp(t, `
- mac: "00:27:22:00:00:02"
  type: usw
  model: USWED74
  modeldisplay: UniFi Switch Edge 74
  version: 4.0.21.9965
  name: core-sw
  ip: 10.0.0.3
  ports: 26
- mac: "00:27:22:e0:00:51"
  model: U7MP
  ssids: [CorpWiFi, Guest]
`)
	got, err := loadDevices(f, "")
	if err != nil {
		t.Fatalf("loadDevices: %v", err)
	}
	want := []emu.DeviceSpec{
		{
			MAC: "00:27:22:00:00:02", Type: "usw", Model: "USWED74",
			ModelDisplay: "UniFi Switch Edge 74", Version: "4.0.21.9965",
			Name: "core-sw", IP: "10.0.0.3", Ports: 26,
		},
		{MAC: "00:27:22:e0:00:51", Model: "U7MP", SSIDs: []string{"CorpWiFi", "Guest"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLoadDevicesJSONFile(t *testing.T) {
	// The itest fleet file is JSON and must keep parsing: JSON is a YAML
	// subset. Reading the real file keeps that guarantee honest.
	got, err := loadDevices("../../scripts/devices.fleet.json", "")
	if err != nil {
		t.Fatalf("loadDevices: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d devices, want 5", len(got))
	}
	first := emu.DeviceSpec{MAC: "00:27:22:e0:00:01", Model: "UGW3", IP: "192.168.1.242"}
	if !reflect.DeepEqual(got[0], first) {
		t.Errorf("got %+v, want %+v", got[0], first)
	}
}

func TestLoadDevicesUnknownField(t *testing.T) {
	// Strictness parity with the old json.Decoder.DisallowUnknownFields:
	// a misspelled key fails loudly in both formats instead of silently
	// dropping to the profile default.
	for name, content := range map[string]string{
		"yaml": "- mac: \"00:27:22:00:00:02\"\n  modle: USWED74\n",
		"json": "[{\"mac\": \"00:27:22:00:00:02\", \"modle\": \"USWED74\"}]",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadDevices(writeTemp(t, content), "")
			if err == nil || !strings.Contains(err.Error(), "modle") {
				t.Errorf("err = %v, want unknown-field error naming modle", err)
			}
		})
	}
}

func TestLoadDevicesEnvInline(t *testing.T) {
	got, err := loadDevices("", composeFleet)
	if err != nil {
		t.Fatalf("loadDevices: %v", err)
	}
	if !reflect.DeepEqual(got, composeSpecs) {
		t.Errorf("got %+v, want %+v", got, composeSpecs)
	}
}

func TestLoadDevicesNone(t *testing.T) {
	// No fleet source: (nil, nil) so the caller falls back to the
	// single-device flags. Whitespace-only env counts as unset.
	for _, env := range []string{"", "  \n\t "} {
		specs, err := loadDevices("", env)
		if err != nil || specs != nil {
			t.Errorf("loadDevices(%q, %q) = %v, %v; want nil, nil", "", env, specs, err)
		}
	}
}

func TestLoadDevicesMalformed(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		_, err := loadDevices("", "- {mac: ")
		if err == nil || !strings.Contains(err.Error(), "SIM_DEVICES") {
			t.Errorf("err = %v, want parse error naming SIM_DEVICES", err)
		}
	})
	t.Run("file", func(t *testing.T) {
		f := writeTemp(t, "- {mac: ")
		_, err := loadDevices(f, "")
		if err == nil || !strings.Contains(err.Error(), f) {
			t.Errorf("err = %v, want parse error naming %s", err, f)
		}
	})
}

func TestLoadDevicesEmpty(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		_, err := loadDevices("", "[]")
		if err == nil || !strings.Contains(err.Error(), "no devices") {
			t.Errorf("err = %v, want no devices", err)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		_, err := loadDevices(writeTemp(t, ""), "")
		if err == nil || !strings.Contains(err.Error(), "no devices") {
			t.Errorf("err = %v, want no devices", err)
		}
	})
	t.Run("whitespace-only file", func(t *testing.T) {
		_, err := loadDevices(writeTemp(t, "  \n  \n"), "")
		if err == nil || !strings.Contains(err.Error(), "no devices") {
			t.Errorf("err = %v, want no devices", err)
		}
	})
	t.Run("multi-document yaml rejected", func(t *testing.T) {
		_, err := loadDevices(writeTemp(t, "- {mac: \"00:27:22:e0:00:01\", model: UGW3}\n---\n- {mac: \"00:27:22:e0:00:02\", model: UGW3}\n"), "")
		if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
			t.Errorf("err = %v, want multiple-documents error", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := loadDevices(filepath.Join(t.TempDir(), "nope.yaml"), "")
		if err == nil || !strings.Contains(err.Error(), "read") {
			t.Errorf("err = %v, want read error", err)
		}
	})
}

func TestFleetSpecsPrecedence(t *testing.T) {
	fleet := writeTemp(t, composeFleet)

	t.Run("both fleet sources is ambiguous", func(t *testing.T) {
		_, _, err := fleetSpecs(fleet, composeFleet, nil)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("err = %v, want ambiguous", err)
		}
	})
	t.Run("file rejects single-device flags", func(t *testing.T) {
		_, _, err := fleetSpecs(fleet, "", map[string]bool{"mac": true})
		if err == nil || !strings.Contains(err.Error(), "-devices cannot be combined with -mac") {
			t.Errorf("err = %v, want -devices/-mac conflict", err)
		}
	})
	t.Run("env overrides single-device flags", func(t *testing.T) {
		specs, ignored, err := fleetSpecs("", composeFleet, map[string]bool{"mac": true, "model": true})
		if err != nil {
			t.Fatalf("fleetSpecs: %v", err)
		}
		if !reflect.DeepEqual(specs, composeSpecs) {
			t.Errorf("specs = %+v, want %+v", specs, composeSpecs)
		}
		if !reflect.DeepEqual(ignored, []string{"mac", "model"}) {
			t.Errorf("ignored = %v, want [mac model]", ignored)
		}
	})
	t.Run("file only", func(t *testing.T) {
		specs, ignored, err := fleetSpecs(fleet, "", map[string]bool{})
		if err != nil {
			t.Fatalf("fleetSpecs: %v", err)
		}
		if !reflect.DeepEqual(specs, composeSpecs) || len(ignored) != 0 {
			t.Errorf("got %v, %v; want %v, []", specs, ignored, composeSpecs)
		}
	})
	t.Run("neither source is single-device mode", func(t *testing.T) {
		specs, ignored, err := fleetSpecs("", "", map[string]bool{"mac": true})
		if err != nil || specs != nil || ignored != nil {
			t.Errorf("got %v, %v, %v; want nil, nil, nil", specs, ignored, err)
		}
	})
}
