package inform

import (
	"strings"
	"testing"
)

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

// A gateway's if_table lists its layer-3 interfaces -- the WAN uplinks --
// not its ports: the LAN ports sit behind a bridge and never appear. The
// entry carries the uplink's own media speed, the port it sits on, and the
// default route learned through it, which is what the controller builds the
// uplink record from.
func TestGatewayIfTableListsTheUplinkInterfaces(t *testing.T) {
	s := NewSession(uxgDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)

	ift := m["if_table"].([]any)
	if len(ift) != 1 {
		t.Fatalf("if_table has %d entries, want one per uplink port (the LAN ports are bridged): %v", len(ift), ift)
	}
	e := ift[0].(map[string]any)
	if e["name"] != "eth0" || e["speed"] != float64(2500) {
		t.Errorf("uplink entry = %v, want eth0 at the uplink's 2.5GbE", e)
	}
	if e["comment"] != "WAN" {
		t.Errorf("comment = %v, want the port label", e["comment"])
	}
	if pp, _ := e["physical_ports"].([]any); len(pp) != 1 || pp[0] != float64(1) {
		t.Errorf("physical_ports = %v, want [1]", e["physical_ports"])
	}
	if gw, _ := e["gateways"].([]any); len(gw) == 0 {
		t.Errorf("uplink entry carries no gateways: %v", e)
	}
	if _, ok := e["rx_multicast"]; !ok {
		t.Errorf("entry lacks rx_multicast, one of the counters the controller copies")
	}
}

// A switch reports one interface, its management interface, and says how
// many ports sit behind it: a real 32-port aggregation switch sends a single
// eth0 row with num_port 32, not thirty-two rows. One row per port would be
// thirty-two interfaces that all share one address.
func TestSwitchIfTableIsTheManagementInterface(t *testing.T) {
	d := uswDesc()
	d.Ports = []Port{
		{IfName: "eth0", Name: "Port 1", PortIdx: 1, Media: "SFP+", IsUplink: true},
		{IfName: "eth1", Name: "Port 2", PortIdx: 2, Media: "GE"},
		{IfName: "eth2", Name: "Port 3", PortIdx: 3, Media: "GE"},
		{IfName: "eth3", Name: "Port 4", PortIdx: 4, Media: "GE"},
	}
	s := NewSession(d, testInformURL, testClock)
	adopt(s)
	m := decode(t, s)

	ift := m["if_table"].([]any)
	if len(ift) != 1 {
		t.Fatalf("if_table has %d entries, want the one management interface", len(ift))
	}
	e := ift[0].(map[string]any)
	if e["name"] != "eth0" || e["num_port"] != float64(4) || e["speed"] != float64(10000) {
		t.Errorf("management entry = %v, want eth0, num_port 4, at the uplink's 10GbE", e)
	}
	// The route fields belong to a gateway's WAN interface; a switch
	// does not send them.
	for _, k := range []string{"gateways", "nameservers", "comment", "physical_ports"} {
		if _, present := e[k]; present {
			t.Errorf("switch interface carries %s, a gateway WAN field", k)
		}
	}
}

// network_table is the gateway's view of its networks, keyed by interface
// as a real one keys them: a row per WAN interface, named after it, and the
// LAN as the bridge br0 with CIDR addresses. The controller reads br0's
// addresses as the device's IPv6 list. Switches and APs do not send it.
func TestGatewayReportsNetworkTable(t *testing.T) {
	s := NewSession(uxgDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)

	nt, ok := m["network_table"].([]any)
	if !ok || len(nt) != 2 {
		t.Fatalf("network_table = %v, want a WAN row and br0", m["network_table"])
	}
	rows := map[string]map[string]any{}
	for _, r := range nt {
		e := r.(map[string]any)
		rows[e["name"].(string)] = e
	}
	if _, ok := rows["eth0"]; !ok {
		t.Errorf("no row named after the uplink interface: %v", rows)
	}
	lan, ok := rows["br0"]
	if !ok {
		t.Fatalf("no br0 bridge row: %v", rows)
	}
	addrs, _ := lan["addresses"].([]any)
	if len(addrs) == 0 || !strings.Contains(addrs[0].(string), "/") {
		t.Errorf("br0 addresses = %v, want CIDR strings", lan["addresses"])
	}
	// Link fields are strings on the wire, as a real gateway sends them.
	for _, k := range []string{"speed", "mtu", "autoneg", "duplex"} {
		if _, isString := lan[k].(string); !isString {
			t.Errorf("br0 %s = %v (%T), want a string", k, lan[k], lan[k])
		}
	}
	for name, desc := range map[string]Descriptor{"switch": uswDesc(), "ap": uapDesc()} {
		s := NewSession(desc, testInformURL, testClock)
		adopt(s)
		if _, present := decode(t, s)["network_table"]; present {
			t.Errorf("%s sends network_table, a gateway table", name)
		}
	}
}

// A gateway claims its features twice, as a real one does: as usg_caps and
// as the has_* booleans the controller ORs back into the bitmap. It claims
// only what it honours -- a default route distance and a disableable SSH
// server -- since every claimed feature is offered against the device.
func TestGatewayReportsUSGCapsAndTheMatchingFlags(t *testing.T) {
	d := uxgDesc()
	d.USGCaps = USGCapDefaultRouteDistance | USGCapSSHDisable
	s := NewSession(d, testInformURL, testClock)
	adopt(s)
	m := decode(t, s)
	if m["usg_caps"] != float64(USGCapDefaultRouteDistance|USGCapSSHDisable) {
		t.Errorf("usg_caps = %v, want %d", m["usg_caps"], USGCapDefaultRouteDistance|USGCapSSHDisable)
	}
	if m["has_default_route_distance"] != true || m["has_ssh_disable"] != true {
		t.Errorf("has_default_route_distance = %v, has_ssh_disable = %v; want both true to match the bitmap",
			m["has_default_route_distance"], m["has_ssh_disable"])
	}

	// Zero means none of the three keys, not three false claims.
	s = NewSession(uxgDesc(), testInformURL, testClock)
	adopt(s)
	m = decode(t, s)
	for _, k := range []string{"usg_caps", "has_default_route_distance", "has_ssh_disable"} {
		if _, present := m[k]; present {
			t.Errorf("%s present on a gateway claiming no usg caps", k)
		}
	}
	// And it is a gateway field: a switch given one does not send it.
	sw := uswDesc()
	sw.USGCaps = USGCapSSHDisable
	s = NewSession(sw, testInformURL, testClock)
	adopt(s)
	if _, present := decode(t, s)["usg_caps"]; present {
		t.Error("switch sends usg_caps, a gateway bitmap")
	}
}

// hw_caps is what the device physically has, and the controller reads it on
// every path. It used to go out only with the power tables, so a gateway
// with a screen or a switch with an RPS port never said so.
func TestHardwareCapsReportedOnEveryType(t *testing.T) {
	gw := uxgDesc()
	gw.HWCaps = HWCapLCM
	sw := uswDesc()
	sw.HWCaps = HWCapRPS
	ap := uapDesc()
	ap.HWCaps = 2048 // 802.3af, the class of power an AP takes
	for name, tc := range map[string]struct {
		desc Descriptor
		want int
	}{"gateway": {gw, HWCapLCM}, "switch": {sw, HWCapRPS}, "ap": {ap, 2048}} {
		s := NewSession(tc.desc, testInformURL, testClock)
		adopt(s)
		if got := decode(t, s)["hw_caps"]; got != float64(tc.want) {
			t.Errorf("%s hw_caps = %v, want %d", name, got, tc.want)
		}
	}
	s := NewSession(uxgDesc(), testInformURL, testClock)
	adopt(s)
	if _, present := decode(t, s)["hw_caps"]; present {
		t.Error("hw_caps present on a device with no hardware bits; zero should be omitted")
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
	// The link settings a real gateway reports alongside the type.
	if wan["autoneg"] != true || wan["full_duplex"] != true || wan["speed"] != "auto" {
		t.Errorf("config_network_wan link settings = %v, want autoneg, full duplex, speed auto", wan)
	}
	if opts, ok := wan["dhcp_options"].([]any); !ok || len(opts) != 0 {
		t.Errorf("dhcp_options = %v, want present and empty", wan["dhcp_options"])
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
