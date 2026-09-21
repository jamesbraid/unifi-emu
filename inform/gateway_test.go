package inform

import "testing"

// uxgDesc is a multi-port gateway with mixed media, the shape the single
// hardcoded gigabit uplink used to flatten.
func uxgDesc() Descriptor {
	return Descriptor{
		MAC: "f4:e2:c6:00:00:01", Serial: "F4E2C6000001", Model: "UXGENT",
		ModelDisplay: "Gateway Enterprise", Version: "5.0.16", IP: "10.0.0.1", Hostname: "UBNT",
		Type: "uxg", FWCaps: PlaceholderFWCaps,
		Ports: []Port{
			{IfName: "eth0", Name: "WAN", PortIdx: 1, Media: "2.5GbE", IsUplink: true},
			{IfName: "eth1", Name: "LAN", PortIdx: 2, Media: "2.5GbE"},
			{IfName: "eth2", Name: "LAN", PortIdx: 3, Media: "10GbE"},
		},
	}
}

// uplink carries the *name* of an interface in if_table, not an object. The
// controller looks the name up and builds its own uplink record from the entry
// it finds; an object is silently discarded and the device ends up with no
// uplink at all, which is a day-costing failure because nothing logs it.
func TestGatewayUplinkIsAnInterfaceName(t *testing.T) {
	s := NewSession(uxgDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)

	uplink, ok := m["uplink"].(string)
	if !ok {
		t.Fatalf("uplink is %T, want a string naming an if_table entry: %v", m["uplink"], m["uplink"])
	}
	if uplink != "eth0" {
		t.Errorf("uplink = %q, want the uplink port's interface name", uplink)
	}

	// The name has to resolve against if_table, or there is nothing for the
	// controller to build the uplink record from.
	ift, ok := m["if_table"].([]any)
	if !ok || len(ift) == 0 {
		t.Fatalf("if_table missing or empty: %v", m["if_table"])
	}
	var found bool
	for _, e := range ift {
		if e.(map[string]any)["name"] == uplink {
			found = true
		}
	}
	if !found {
		t.Errorf("uplink %q names no entry in if_table", uplink)
	}
}

// The gateway branch used to report one gigabit eth0 whatever the model was,
// so a six-port 10GbE gateway looked like a single gigabit port.
func TestGatewayIfTableFollowsTheModelLayout(t *testing.T) {
	s := NewSession(uxgDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)

	ift := m["if_table"].([]any)
	if len(ift) != 3 {
		t.Fatalf("if_table has %d entries, want one per port", len(ift))
	}
	want := map[string]float64{"eth0": 2500, "eth1": 2500, "eth2": 10000}
	for _, e := range ift {
		entry := e.(map[string]any)
		name := entry["name"].(string)
		if got := entry["speed"]; got != want[name] {
			t.Errorf("%s speed = %v, want %v", name, got, want[name])
		}
	}
}

// Absent config_network_wan the controller logs the missing key and skips WAN
// processing entirely, so the WAN never bootstraps and the gateway adopts into
// a half-configured state.
func TestGatewayAlwaysReportsWANConfig(t *testing.T) {
	s := NewSession(uxgDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)
	wan, ok := m["config_network_wan"].(map[string]any)
	if !ok {
		t.Fatalf("config_network_wan missing: %v", m["config_network_wan"])
	}
	if wan["type"] != "dhcp" {
		t.Errorf("config_network_wan type = %v, want dhcp", wan["type"])
	}
}

// system-stats is read with an integer accessor that parses a decimal integer,
// so a fractional string reads as zero and the gateway looks permanently idle.
func TestGatewaySystemStatsAreWholeNumbers(t *testing.T) {
	s := NewSession(uxgDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)
	stats := m["system-stats"].(map[string]any)
	for _, f := range []string{"cpu", "mem", "uptime"} {
		v, ok := stats[f].(string)
		if !ok {
			t.Errorf("system-stats %q is %T, want a string", f, stats[f])
			continue
		}
		for _, r := range v {
			if r < '0' || r > '9' {
				t.Errorf("system-stats %q = %q, which an integer accessor reads as 0", f, v)
				break
			}
		}
	}
}

// A device that reports no if_table has nothing for its uplink name to resolve
// against, so it adopts and reports normally but hangs off nothing: no uplink,
// no parent device. Gateways and switches both need one -- and the power
// lineup reports as switches, so this is what gives a PDU its uplink.
func TestWiredDevicesReportAnInterfaceTable(t *testing.T) {
	for name, desc := range map[string]Descriptor{
		"gateway": uxgDesc(),
		"switch":  uswDesc(),
		"pdu":     pduDesc(),
	} {
		t.Run(name, func(t *testing.T) {
			s := NewSession(desc, testInformURL, testClock)
			adopt(s)
			m := decode(t, s)

			ift, ok := m["if_table"].([]any)
			if !ok || len(ift) == 0 {
				t.Fatalf("if_table missing or empty: %v", m["if_table"])
			}
			uplink, ok := m["uplink"].(string)
			if !ok {
				t.Fatalf("uplink is %T, want a string naming an if_table entry", m["uplink"])
			}
			var found bool
			for _, e := range ift {
				if e.(map[string]any)["name"] == uplink {
					found = true
				}
			}
			if !found {
				t.Errorf("uplink %q names no entry in if_table", uplink)
			}
		})
	}
}

// An AP's wired port is not an uplink the controller resolves this way, and
// inventing one would change a payload shape real controllers already accept.
func TestAccessPointsHaveNoInterfaceTable(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)
	if _, present := m["if_table"]; present {
		t.Errorf("if_table present on an access point")
	}
}
