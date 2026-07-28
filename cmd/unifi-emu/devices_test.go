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
		_, _, err := fleetSpecs(fleetSources{devicesFile: fleet, envInline: composeFleet}, nil)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("err = %v, want ambiguous", err)
		}
	})
	t.Run("file rejects single-device flags", func(t *testing.T) {
		_, _, err := fleetSpecs(fleetSources{devicesFile: fleet}, map[string]bool{"mac": true})
		if err == nil || !strings.Contains(err.Error(), "-devices cannot be combined with -mac") {
			t.Errorf("err = %v, want -devices/-mac conflict", err)
		}
	})
	t.Run("env overrides single-device flags", func(t *testing.T) {
		specs, ignored, err := fleetSpecs(fleetSources{envInline: composeFleet}, map[string]bool{"mac": true, "model": true})
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
		specs, ignored, err := fleetSpecs(fleetSources{devicesFile: fleet}, map[string]bool{})
		if err != nil {
			t.Fatalf("fleetSpecs: %v", err)
		}
		if !reflect.DeepEqual(specs, composeSpecs) || len(ignored) != 0 {
			t.Errorf("got %v, %v; want %v, []", specs, ignored, composeSpecs)
		}
	})
	t.Run("neither source is single-device mode", func(t *testing.T) {
		specs, ignored, err := fleetSpecs(fleetSources{}, map[string]bool{"mac": true})
		if err != nil || specs != nil || ignored != nil {
			t.Errorf("got %v, %v, %v; want nil, nil, nil", specs, ignored, err)
		}
	})
	t.Run("models flag rejects single-device flags", func(t *testing.T) {
		_, _, err := fleetSpecs(fleetSources{modelsFlag: "U7PRO"}, map[string]bool{"mac": true})
		if err == nil || !strings.Contains(err.Error(), "-models cannot be combined with -mac") {
			t.Errorf("err = %v, want -models/-mac conflict", err)
		}
	})
	t.Run("SIM_MODELS overrides single-device flags", func(t *testing.T) {
		specs, ignored, err := fleetSpecs(fleetSources{models: "U7PRO"}, map[string]bool{"mac": true})
		if err != nil {
			t.Fatalf("fleetSpecs: %v", err)
		}
		if len(specs) != 1 || specs[0].Model != "U7PRO" {
			t.Errorf("specs = %+v, want one U7PRO spec", specs)
		}
		if !reflect.DeepEqual(ignored, []string{"mac"}) {
			t.Errorf("ignored = %v, want [mac]", ignored)
		}
	})
	t.Run("three fleet sources named in error", func(t *testing.T) {
		_, _, err := fleetSpecs(fleetSources{devicesFile: fleet, envInline: composeFleet, modelsFlag: "U7PRO"}, nil)
		if err == nil {
			t.Fatal("three fleet sources accepted, want error")
		}
		for _, want := range []string{"-devices", "SIM_DEVICES", "-models"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to name %s", err, want)
			}
		}
	})
}

func TestFleetModelsFlagAndEnvConflict(t *testing.T) {
	_, _, err := fleetSpecs(fleetSources{modelsFlag: "U7PRO", models: "USM8P"}, nil)
	if err == nil {
		t.Fatal("-models and SIM_MODELS both set: err=nil, want error")
	}
}

func TestFleetSourcesMutuallyExclusive(t *testing.T) {
	// -devices file and -models flag are both fleet sources; both set is ambiguous.
	set := map[string]bool{"models": true}
	_, _, err := fleetSpecs(fleetSources{devicesFile: "f.json", models: "U7PRO"}, set)
	if err == nil {
		t.Fatal("two fleet sources accepted, want error")
	}
}

func TestFleetModelsResolves(t *testing.T) {
	specs, _, err := fleetSpecs(fleetSources{models: "U7PRO,USM8P:2"}, map[string]bool{"models": true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(specs) != 3 {
		t.Fatalf("got %d specs, want 3", len(specs))
	}
}

func TestParseModelsList(t *testing.T) {
	got, err := parseModelsList(" U7PRO, USM8P:2 ,UGW3 ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"U7PRO", "USM8P", "USM8P", "UGW3"}
	if len(got) != len(want) {
		t.Fatalf("got %d specs, want %d: %+v", len(got), len(want), got)
	}
	for i, m := range want {
		if got[i].Model != m {
			t.Fatalf("spec[%d].Model = %q, want %q", i, got[i].Model, m)
		}
	}
}

func TestParseModelsListErrors(t *testing.T) {
	for _, in := range []string{"", "   ", "U7PRO:0", "U7PRO:-1", "U7PRO:x", "U7PRO:2:3"} {
		if _, err := parseModelsList(in); err == nil {
			t.Errorf("parseModelsList(%q) err=nil, want error", in)
		}
	}
}

func TestExpandFleetDefaults(t *testing.T) {
	in := []emu.DeviceSpec{{Model: "U7PRO"}, {Model: "USM8P"}}
	out, err := expandFleet(in, "00:27:22:e0:00:00", "192.168.1.100")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if out[0].MAC != "00:27:22:e0:00:01" || out[1].MAC != "00:27:22:e0:00:02" {
		t.Fatalf("MACs: %q %q", out[0].MAC, out[1].MAC)
	}
	if out[0].IP != "192.168.1.100" || out[1].IP != "192.168.1.101" {
		t.Fatalf("IPs: %q %q", out[0].IP, out[1].IP)
	}
}

func TestExpandFleetExplicitWins(t *testing.T) {
	in := []emu.DeviceSpec{{Model: "U7PRO", MAC: "aa:bb:cc:dd:ee:ff", IP: "10.0.0.9"}}
	out, err := expandFleet(in, "00:27:22:e0:00:00", "192.168.1.100")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if out[0].MAC != "aa:bb:cc:dd:ee:ff" || out[0].IP != "10.0.0.9" {
		t.Fatalf("explicit overridden: %+v", out[0])
	}
}

// A site adopts one gateway whichever family it comes from, so ugw and uxg
// compete for the same slot. Counting only ugw let a mixed pair through here
// and left the controller to reject it minutes into a live run.
func TestExpandFleetRejectsTwoGateways(t *testing.T) {
	for _, models := range [][]string{
		{"UGW3", "UGW3"},
		{"UGW3", "UXGENT"},
		{"UXGENT", "UGW3"},
		{"UXGENT", "UXGB"},
	} {
		in := []emu.DeviceSpec{{Model: models[0]}, {Model: models[1]}}
		if _, err := expandFleet(in, "00:27:22:e0:00:00", "192.168.1.100"); err == nil {
			t.Errorf("%v accepted, want the one-gateway error", models)
		}
	}
}

// One gateway of either family beside non-gateways stays legal.
func TestExpandFleetAcceptsOneGateway(t *testing.T) {
	for _, gateway := range []string{"UGW3", "UXGENT"} {
		in := []emu.DeviceSpec{{Model: gateway}, {Model: "US24"}}
		if _, err := expandFleet(in, "00:27:22:e0:00:00", "192.168.1.100"); err != nil {
			t.Errorf("%s with a switch rejected: %v", gateway, err)
		}
	}
}

func TestExpandFleetUnknownModel(t *testing.T) {
	in := []emu.DeviceSpec{{Model: "NOPE"}}
	if _, err := expandFleet(in, "00:27:22:e0:00:00", "192.168.1.100"); err == nil {
		t.Fatal("unknown model accepted, want error")
	}
}

func TestExpandFleetIPOverflow(t *testing.T) {
	in := make([]emu.DeviceSpec, 3)
	for i := range in {
		in[i] = emu.DeviceSpec{Model: "U7PRO"}
	}
	if _, err := expandFleet(in, "00:27:22:e0:00:00", "192.168.1.254"); err == nil {
		t.Fatal("IP past .254 accepted, want error")
	}
}

func TestDefaultFleetLoadsWhenNothingSet(t *testing.T) {
	// The baked-image path: no single-device flag set, a default file
	// present -> its fleet loads. This is what `docker run img` hits.
	specs, err := defaultFleet("../../scripts/devices.fleet.json", map[string]bool{})
	if err != nil {
		t.Fatalf("defaultFleet: %v", err)
	}
	if len(specs) != 5 {
		t.Fatalf("got %d devices, want the 5-device baked fleet", len(specs))
	}
}

func TestDefaultFleetEmptyPathNoop(t *testing.T) {
	// Outside the image SIM_DEFAULT_DEVICES is unset: no default, and the
	// caller keeps single-device mode.
	specs, err := defaultFleet("", map[string]bool{})
	if err != nil || specs != nil {
		t.Errorf("defaultFleet(\"\") = %v, %v; want nil, nil", specs, err)
	}
}

func TestDefaultFleetSuppressedBySingleFlag(t *testing.T) {
	// An explicit single-device flag means the user chose one device; the
	// baked default must not override that.
	for _, f := range singleFlags {
		specs, err := defaultFleet("../../scripts/devices.fleet.json", map[string]bool{f: true})
		if err != nil || specs != nil {
			t.Errorf("defaultFleet with -%s set = %v, %v; want nil, nil", f, specs, err)
		}
	}
}

func TestFleetSpecsAmbiguousDevicesJSON(t *testing.T) {
	src := fleetSources{
		envInline:   "[{}]",
		devicesJSON: "[{}]",
	}
	_, _, err := fleetSpecs(src, nil)
	if err == nil || !strings.Contains(err.Error(), "ambiguous device sources") {
		t.Fatalf("expected ambiguity error, got: %v", err)
	}

	src2 := fleetSources{
		models:      "U7PRO",
		devicesJSON: "[{}]",
	}
	_, _, err = fleetSpecs(src2, nil)
	if err == nil || !strings.Contains(err.Error(), "ambiguous device sources") {
		t.Fatalf("expected ambiguity error, got: %v", err)
	}
}

func TestFleetSpecsUNIFI_EMU_DEVICES_JSON(t *testing.T) {
	devicesJSON := `[{"mac": "00:27:22:e0:00:01", "model": "U7PRO", "serial": "JSONSERIAL123"}]`
	src := fleetSources{
		devicesJSON: devicesJSON,
	}
	specs, ignored, err := fleetSpecs(src, nil)
	if err != nil {
		t.Fatalf("fleetSpecs error: %v", err)
	}
	if len(ignored) != 0 {
		t.Fatalf("expected no ignored flags, got: %v", ignored)
	}
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got: %d", len(specs))
	}
	if specs[0].MAC != "00:27:22:e0:00:01" || specs[0].Model != "U7PRO" || specs[0].Serial != "JSONSERIAL123" {
		t.Fatalf("unexpected spec: %+v", specs[0])
	}
}
