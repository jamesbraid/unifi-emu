package inform

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

const testInformURL = "http://unifi:8080/inform"

var testClock = time.Unix(1_700_000_000, 0)

// uapDesc/uswDesc/ugwDesc are minimal descriptors for shape tests. Real
// registry-backed descriptors are exercised from emu's TestModelRegistryPayloads.
func uswDesc() Descriptor {
	return Descriptor{
		MAC: "00:27:22:00:00:02", Serial: "002722000002", Model: "USWED74",
		ModelDisplay: "Switch", Version: "1.2.3", IP: "10.0.0.3", Hostname: "UBNT",
		Type: "usw", FWCaps: PlaceholderFWCaps,
		Ports: []Port{{IfName: "eth0", Name: "Port 1", PortIdx: 1, Media: "GE", IsUplink: true}},
	}
}
func uapDesc(ssids ...string) Descriptor {
	return Descriptor{
		MAC: "00:15:6d:00:00:01", Serial: "00156D000001", Model: "U7MP",
		ModelDisplay: "AP", Version: "6.8.2", IP: "10.0.0.57", Hostname: "UBNT",
		Type: "uap", FWCaps: PlaceholderFWCaps, SSIDs: ssids,
		Radios: []Radio{
			{Name: "wifi0", Radio: "ng", Channel: 6, HT: "20", MaxTxPower: 20, NSS: 2, RadioCaps: 1},
			{Name: "wifi1", Radio: "na", Channel: 36, HT: "40", MaxTxPower: 22, NSS: 2, RadioCaps: 1},
		},
	}
}
func ugwDesc() Descriptor {
	return Descriptor{
		MAC: "dc:9f:db:00:00:01", Serial: "DC9FDB000001", Model: "UGW3",
		ModelDisplay: "Gateway", Version: "4.4.57", IP: "10.0.0.1", Hostname: "UBNT",
		Type: "ugw", FWCaps: PlaceholderFWCaps,
		Ports: []Port{{IfName: "eth0", Name: "Port 1", PortIdx: 1, Media: "GE", IsUplink: true}},
	}
}

func decode(t *testing.T, s *Session) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(s.BuildPayload(testClock), &m); err != nil {
		t.Fatalf("BuildPayload is not valid JSON: %v", err)
	}
	return m
}

// adopt flips the session into the adopted state for payload-shape tests.
// This is a stand-in for the real set-adopt path through Session.Apply,
// which lands in Task 3; it is restored there to call s.Apply(testClock,
// []byte(`{"_type":"cmd","cmd":"set-adopt"}`)) so these tests exercise the
// adopted branch the same way production reaches it.
func adopt(s *Session) { s.adopted = true }

func TestPendingPayloadCommon(t *testing.T) {
	s := NewSession(uswDesc(), testInformURL, testClock)
	m := decode(t, s)
	want := map[string]any{
		"mac": "00:27:22:00:00:02", "serial": "002722000002", "model": "USWED74",
		"ip": "10.0.0.3", "inform_url": testInformURL, "cfgversion": "0",
		"state": float64(1), "default": true, "_default_key": true, "x_authkey": DefaultKey,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if _, ok := m["uptime"].(float64); !ok {
		t.Errorf("uptime missing or not numeric: %v", m["uptime"])
	}
}

func TestAdoptedPayloadUSW(t *testing.T) {
	s := NewSession(uswDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)
	if m["state"] != float64(4) {
		t.Errorf("state = %v, want 4", m["state"])
	}
	pt, ok := m["port_table"].([]any)
	if !ok || len(pt) == 0 {
		t.Fatalf("port_table missing or empty: %v", m["port_table"])
	}
	if _, ok := m["system-stats"]; ok {
		t.Errorf("system-stats present on a switch; that is ugw-only")
	}
}

func TestAdoptedPayloadUAPEmptyVaps(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)
	rt, ok := m["radio_table"].([]any)
	if !ok || len(rt) < 2 {
		t.Fatalf("radio_table has %v, want >= 2 entries", m["radio_table"])
	}
	vaps, ok := m["vap_table"].([]any)
	if !ok || len(vaps) != 0 {
		t.Fatalf("default vap_table = %v, want present but empty", m["vap_table"])
	}
}

func TestAdoptedPayloadUAPWithSSIDs(t *testing.T) {
	s := NewSession(uapDesc("CorpWiFi"), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)
	vaps, _ := m["vap_table"].([]any)
	if len(vaps) == 0 {
		t.Fatal("vap_table empty, want a vap per SSID per radio")
	}
	bssid := vaps[0].(map[string]any)["bssid"].(string)
	hw, err := net.ParseMAC(bssid)
	if err != nil || hw[0]&0x02 == 0 {
		t.Errorf("bssid %q must parse and be locally administered", bssid)
	}
}

func TestAdoptedPayloadUGW(t *testing.T) {
	s := NewSession(ugwDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)
	stats, ok := m["system-stats"].(map[string]any)
	if !ok {
		t.Fatalf("system-stats missing: %v", m["system-stats"])
	}
	for _, f := range []string{"cpu", "mem", "uptime"} {
		if _, ok := stats[f]; !ok {
			t.Errorf("system-stats missing %q", f)
		}
	}
}

func TestUDAPIKeysPairing(t *testing.T) {
	d := ugwDesc()
	d.UDAPIVersion = "9.0"
	d.UDAPICaps = 1 << 22
	s := NewSession(d, testInformURL, testClock)
	m := decode(t, s)
	if _, ok := m["udapi_version"]; !ok {
		t.Error("udapi_version missing when UDAPIVersion set")
	}
	if m["udapi_caps"] != float64(1<<22) {
		t.Errorf("udapi_caps = %v, want %d", m["udapi_caps"], 1<<22)
	}
	// Neither key when UDAPIVersion is empty.
	s2 := NewSession(ugwDesc(), testInformURL, testClock)
	m2 := decode(t, s2)
	if _, ok := m2["udapi_caps"]; ok {
		t.Error("udapi_caps present with no UDAPIVersion; want neither key")
	}
}
