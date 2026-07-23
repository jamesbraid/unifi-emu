package emu

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

const testInformURL = "http://unifi:8080/inform"

func mustDevice(t *testing.T, spec DeviceSpec) *device {
	t.Helper()
	d, err := newDevice(spec, testInformURL)
	if err != nil {
		t.Fatalf("newDevice(%+v): %v", spec, err)
	}
	return d
}

// markAdopted flips a device into the adopted state for payload-shape tests
// (the real transition path via applyResponse is covered by response_test.go).
func markAdopted(d *device) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.adopted = true
	d.state = StateAdopting
}

func decodePayload(t *testing.T, d *device) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(d.buildPayload(), &m); err != nil {
		t.Fatalf("buildPayload is not valid JSON: %v", err)
	}
	return m
}

func table(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key].([]any)
	if !ok || len(v) == 0 {
		t.Fatalf("%s missing or empty in payload", key)
	}
	return v
}

func requireFields(t *testing.T, where string, entry map[string]any, fields ...string) {
	t.Helper()
	for _, f := range fields {
		if _, ok := entry[f]; !ok {
			t.Errorf("%s entry missing field %q: %v", where, f, entry)
		}
	}
}

func TestPendingPayloadCommon(t *testing.T) {
	specs := []DeviceSpec{
		{MAC: "dc:9f:db:00:00:01", Model: "UGW3", IP: "10.0.0.1"},
		{MAC: "00:27:22:00:00:02", Model: "USWED74", IP: "10.0.0.3"},
		{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"},
	}
	for _, spec := range specs {
		t.Run(spec.Model, func(t *testing.T) {
			d := mustDevice(t, spec)
			m := decodePayload(t, d)

			want := map[string]any{
				"mac":          spec.MAC,
				"serial":       strings.ToUpper(strings.ReplaceAll(spec.MAC, ":", "")),
				"model":        spec.Model,
				"ip":           spec.IP,
				"inform_url":   testInformURL,
				"cfgversion":   "0",
				"state":        float64(1),
				"default":      true,
				"_default_key": true,
				"x_authkey":    DefaultKey,
			}
			for k, v := range want {
				if m[k] != v {
					t.Errorf("%s = %v, want %v", k, m[k], v)
				}
			}
			for _, k := range []string{"serial", "model_display", "version", "hostname"} {
				if s, ok := m[k].(string); !ok || s == "" {
					t.Errorf("%s missing or empty", k)
				}
			}
			if _, ok := m["uptime"].(float64); !ok {
				t.Errorf("uptime missing or not numeric: %v", m["uptime"])
			}
		})
	}
}

func TestAdoptedPayloadUGW(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "dc:9f:db:00:00:01", Model: "UGW3", IP: "10.0.0.1"})
	markAdopted(d)
	m := decodePayload(t, d)
	if _, ok := m["required_version"]; ok {
		t.Errorf("adopted payload advertises a global required_version: %v", m["required_version"])
	}

	if m["state"] != float64(2) {
		t.Errorf("state = %v, want 2", m["state"])
	}
	if m["default"] != false {
		t.Errorf("default = %v, want false", m["default"])
	}
	stats, ok := m["system-stats"].(map[string]any)
	if !ok {
		t.Fatalf("system-stats missing or wrong type: %v", m["system-stats"])
	}
	requireFields(t, "system-stats", stats, "cpu", "mem", "uptime")
	wan, ok := m["config_network_wan"].(map[string]any)
	if !ok {
		t.Fatalf("config_network_wan missing: %v", m["config_network_wan"])
	}
	if wan["type"] != "dhcp" {
		t.Errorf("config_network_wan.type = %v, want dhcp", wan["type"])
	}
	if _, ok := m["sys_stats"].(map[string]any); !ok {
		t.Errorf("sys_stats missing (common adopted field)")
	}
}

func TestAdoptedPayloadUSW(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:27:22:00:00:02", Model: "USWED74", IP: "10.0.0.3"})
	markAdopted(d)
	m := decodePayload(t, d)

	for _, e := range table(t, m, "port_table") {
		entry, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("port_table entry not an object: %v", e)
		}
		requireFields(t, "port_table", entry, "ifname", "port_idx", "media", "up", "speed")
	}
	table(t, m, "ethernet_table")
	if _, ok := m["sys_stats"].(map[string]any); !ok {
		t.Errorf("sys_stats missing (common adopted field)")
	}
	if _, ok := m["system-stats"]; ok {
		t.Errorf("system-stats present on a switch payload; that is ugw-only")
	}
}

func TestAdoptedPayloadUAP(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	markAdopted(d)
	m := decodePayload(t, d)

	rt := table(t, m, "radio_table")
	if len(rt) < 2 {
		t.Fatalf("radio_table has %d entries, want >= 2", len(rt))
	}
	for _, e := range rt {
		entry, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("radio_table entry not an object: %v", e)
		}
		requireFields(t, "radio_table", entry, "name", "radio", "channel", "ht", "nss", "radio_caps")
	}
	// Default vaps: present but empty. This controller build rejects
	// default vaps (their id is not a valid wlanconf ObjectId) with
	// ERROR noise on every inform and drops them, so an AP informs with
	// no vaps until a setstate provisions real WLAN config — the same
	// empty vap_table the accepted oracle AP carries (tmp/oracle-uap.json, gitignored live evidence).
	vaps, ok := m["vap_table"].([]any)
	if !ok {
		t.Fatalf("vap_table missing or not an array in payload")
	}
	if len(vaps) != 0 {
		t.Fatalf("default vap_table has %d entries, want 0 (empty until provisioned)", len(vaps))
	}
	if _, ok := m["sys_stats"].(map[string]any); !ok {
		t.Errorf("sys_stats missing (common adopted field)")
	}
}

// DeviceSpec.SSIDs is the opt-in escape hatch for vaps: explicitly
// listed SSIDs are emitted even though the default payload stays empty.
func TestAdoptedPayloadUAPWithSSIDs(t *testing.T) {
	d := mustDevice(t, DeviceSpec{
		MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57",
		SSIDs: []string{"CorpWiFi"},
	})
	markAdopted(d)
	m := decodePayload(t, d)

	for _, e := range table(t, m, "vap_table") {
		entry, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("vap_table entry not an object: %v", e)
		}
		requireFields(t, "vap_table", entry, "essid", "bssid", "radio", "up", "num_sta")
	}
}

func TestNewDeviceRejectsUnknownModel(t *testing.T) {
	if _, err := newDevice(DeviceSpec{MAC: "00:15:6d:00:00:09", Model: "NOPE"}, testInformURL); err == nil {
		t.Fatal("newDevice with unknown model: want error, got nil")
	}
}

// An explicit Type that contradicts the model profile builds an
// incoherent device (ugw identity with usw payload tables); reject it
// instead of letting a typo'd -type flag inform nonsense.
func TestNewDeviceRejectsMismatchedType(t *testing.T) {
	if _, err := newDevice(DeviceSpec{MAC: "00:15:6d:00:00:09", Model: "UGW3", Type: "usw"}, testInformURL); err == nil {
		t.Fatal("newDevice with Type usw on a ugw model: want error, got nil")
	}
	if _, err := newDevice(DeviceSpec{MAC: "00:15:6d:00:00:09", Model: "UGW3", Type: "ugw"}, testInformURL); err != nil {
		t.Fatalf("newDevice with matching explicit Type: %v", err)
	}
}

func TestDeviceSpecDefaults(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	if d.spec.Type != "uap" {
		t.Errorf("Type = %q, want profile default %q", d.spec.Type, "uap")
	}
	if d.spec.ModelDisplay == "" {
		t.Errorf("ModelDisplay not defaulted from profile")
	}
	if d.spec.Version != "4.0.21.9965" {
		t.Errorf("Version = %q, want profile default 4.0.21.9965", d.spec.Version)
	}
	if d.spec.Name != "UBNT" {
		t.Errorf("Name = %q, want default UBNT", d.spec.Name)
	}
	if d.key != DefaultKey {
		t.Errorf("key = %q, want DefaultKey", d.key)
	}
	if d.cfgvers != "0" {
		t.Errorf("cfgvers = %q, want \"0\"", d.cfgvers)
	}
	if d.interval != 10*time.Second {
		t.Errorf("interval = %v, want 10s", d.interval)
	}
	if d.state != StatePending {
		t.Errorf("state = %v, want PENDING", d.state)
	}
	if d.informURL != testInformURL {
		t.Errorf("informURL = %q, want %q", d.informURL, testInformURL)
	}

	// Type can only ever repeat the profile's (mismatches are rejected),
	// so the explicit-wins probe uses the other overridable fields.
	d2 := mustDevice(t, DeviceSpec{
		MAC: "00:15:6d:00:00:02", Model: "U7MP", IP: "10.0.0.58",
		Type: "uap", ModelDisplay: "Custom Display", Version: "9.9.9", Name: "ap1",
	})
	if d2.spec.Type != "uap" || d2.spec.ModelDisplay != "Custom Display" ||
		d2.spec.Version != "9.9.9" || d2.spec.Name != "ap1" {
		t.Errorf("explicit spec values did not win over profile defaults: %+v", d2.spec)
	}
}

func TestSSIDsOverride(t *testing.T) {
	d := mustDevice(t, DeviceSpec{
		MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57",
		SSIDs: []string{"CorpWiFi", "Guest"},
	})
	markAdopted(d)
	m := decodePayload(t, d)

	got := map[string]bool{}
	for _, e := range table(t, m, "vap_table") {
		entry := e.(map[string]any)
		got[entry["essid"].(string)] = true
	}
	want := map[string]bool{"CorpWiFi": true, "Guest": true}
	if len(got) != len(want) {
		t.Fatalf("vap essids = %v, want %v", got, want)
	}
	for ssid := range want {
		if !got[ssid] {
			t.Errorf("vap essid %q missing, got %v", ssid, got)
		}
	}
}

func TestVapBSSIDsUniqueAcrossAdjacentMACs(t *testing.T) {
	d1 := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57", SSIDs: []string{"CorpWiFi"}})
	d2 := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:02", Model: "U7MP", IP: "10.0.0.58", SSIDs: []string{"CorpWiFi"}})
	markAdopted(d1)
	markAdopted(d2)

	seen := map[string]string{}
	for name, m := range map[string]map[string]any{
		"d1": decodePayload(t, d1),
		"d2": decodePayload(t, d2),
	} {
		for _, e := range table(t, m, "vap_table") {
			bssid, ok := e.(map[string]any)["bssid"].(string)
			if !ok {
				t.Fatalf("%s: vap entry missing bssid", name)
			}
			hw, err := net.ParseMAC(bssid)
			if err != nil {
				t.Fatalf("%s: bssid %q does not parse: %v", name, bssid, err)
			}
			if hw[0]&0x02 == 0 {
				t.Errorf("%s: bssid %q lacks the locally-administered bit", name, bssid)
			}
			if other, dup := seen[bssid]; dup {
				t.Errorf("bssid %q shared by %s and %s", bssid, other, name)
			}
			seen[bssid] = name
		}
	}
}

func TestModelRegistryPayloads(t *testing.T) {
	for model, profile := range modelRegistry {
		t.Run(model, func(t *testing.T) {
			d := mustDevice(t, DeviceSpec{MAC: "02:00:00:00:00:01", Model: model, IP: "10.0.0.99"})
			markAdopted(d)
			m := decodePayload(t, d)
			switch profile.Type {
			case "uap":
				table(t, m, "radio_table")
				// vaps default to empty until provisioned (see
				// TestAdoptedPayloadUAP); the key must still be present.
				if _, ok := m["vap_table"].([]any); !ok {
					t.Errorf("vap_table missing or not an array for uap model %s", model)
				}
			case "usw":
				table(t, m, "port_table")
				table(t, m, "ethernet_table")
			case "ugw":
				stats, ok := m["system-stats"].(map[string]any)
				if !ok || len(stats) == 0 {
					t.Errorf("system-stats missing or empty for ugw model %s", model)
				}
			default:
				t.Errorf("profile %s has unknown type %q", model, profile.Type)
			}
		})
	}
}

func TestDeviceStateString(t *testing.T) {
	cases := map[DeviceState]string{
		StatePending:    "PENDING",
		StateAdopting:   "ADOPTING",
		StateConnected:  "CONNECTED",
		DeviceState(42): "UNKNOWN",
	}
	for state, want := range cases {
		if got := state.String(); got != want {
			t.Errorf("DeviceState(%d).String() = %q, want %q", int(state), got, want)
		}
	}
}
