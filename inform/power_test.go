package inform

import "testing"

// pduDesc is a rack PDU: outlets, a metered bank, and a type of usw, because
// that is what the rack PDUs really report.
func pduDesc() Descriptor {
	return Descriptor{
		MAC: "74:83:c2:00:00:01", Serial: "7483C2000001", Model: "USPPDUP",
		ModelDisplay: "PDU Pro", Version: "7.2.123", IP: "10.0.0.7", Hostname: "UBNT",
		Type: "usw", FWCaps: PlaceholderFWCaps,
		Ports: []Port{{IfName: "eth0", Name: "Port 1", PortIdx: 1, Media: "GE", IsUplink: true}},
		Outlets: []Outlet{
			{Index: 1, Name: "USB Outlet 1", Group: "usb", HasRelay: true,
				Caps: OutletCapHasRelay | OutletCapUSB},
			{Index: 2, Name: "Outlet 2", Group: "standard", HasRelay: true, HasMetering: true,
				Caps: OutletCapHasRelay | OutletCapPowerMeter | OutletCapAC},
		},
	}
}

// upsDesc is a battery-backed device. The NUT bit is what makes the controller
// treat it as a UPS rather than a power strip.
func upsDesc() Descriptor {
	d := pduDesc()
	d.Model, d.ModelDisplay, d.Type = "USWDA23", "UPS Tower", "usp"
	d.SmartPowerCaps = SmartPowerCapNUTInformationAccess | SmartPowerCapBuzzer
	return d
}

func TestAdoptedPayloadPDUOutletTable(t *testing.T) {
	s := NewSession(pduDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)

	ot, ok := m["outlet_table"].([]any)
	if !ok || len(ot) != 2 {
		t.Fatalf("outlet_table missing or wrong length: %v", m["outlet_table"])
	}

	// A non-metering outlet must omit the metering keys rather than report
	// them as zero: the controller distinguishes an absent reading from a
	// real zero, and zeros would show the outlet drawing no power instead
	// of not measuring it.
	usb, _ := ot[0].(map[string]any)
	for _, k := range []string{"outlet_voltage", "outlet_current", "outlet_power"} {
		if _, present := usb[k]; present {
			t.Errorf("non-metering outlet reports %q; it must be omitted", k)
		}
	}
	metered, _ := ot[1].(map[string]any)
	if metered["outlet_voltage"] != float64(120) {
		t.Errorf("metered outlet voltage = %v, want 120", metered["outlet_voltage"])
	}
	if metered["index"] != float64(2) {
		t.Errorf("outlet index = %v, want 2", metered["index"])
	}
}

// A power device is not identified by its type: rack PDUs report as switches
// and smart plugs as access points, so the outlet tables must follow the shape
// the device actually has. A type-gated emission would leave every real PDU
// without outlets.
func TestOutletTableIsNotTypeGated(t *testing.T) {
	for _, typ := range []string{"usw", "uap", "uxg", "usp"} {
		t.Run(typ, func(t *testing.T) {
			d := pduDesc()
			d.Type = typ
			s := NewSession(d, testInformURL, testClock)
			adopt(s)
			m := decode(t, s)
			if _, ok := m["outlet_table"].([]any); !ok {
				t.Errorf("outlet_table missing for a device of type %q that has outlets", typ)
			}
		})
	}
}

// A switch with no outlets must not grow an empty outlet table: an outlet_table
// key is itself the signal that the device is a power device.
func TestNonPowerDeviceHasNoOutletTable(t *testing.T) {
	s := NewSession(uswDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)
	if _, present := m["outlet_table"]; present {
		t.Errorf("outlet_table present on a plain switch: %v", m["outlet_table"])
	}
	if _, present := m["vbms_table"]; present {
		t.Errorf("vbms_table present on a plain switch")
	}
}

// Outlet switching arrives as configuration, not as a command, and the
// controller re-sends the whole config file on every inform until the device
// reports the new state back. A device that never echoes it is pushed the same
// change forever.
func TestSystemCfgOutletRelayIsEchoedBack(t *testing.T) {
	s := NewSession(pduDesc(), testInformURL, testClock)
	adopt(s)
	s.Apply(testClock, []byte(`{"_type":"setparam","system_cfg":"outlet.2.relay_state=disabled\noutlet.status=enabled"}`))

	m := decode(t, s)
	ot := m["outlet_table"].([]any)
	if got := ot[1].(map[string]any)["relay_state"]; got != false {
		t.Errorf("outlet 2 relay_state = %v after a disable push, want false", got)
	}
	// The untouched outlet keeps its default rather than following its
	// neighbour.
	if got := ot[0].(map[string]any)["relay_state"]; got != true {
		t.Errorf("outlet 1 relay_state = %v, want it left alone", got)
	}
}

// A factory reset has to drop pushed outlet state with the rest of the
// provisioning; otherwise a re-adopted device reports relay positions the new
// controller never set.
func TestSetdefaultClearsOutletState(t *testing.T) {
	s := NewSession(pduDesc(), testInformURL, testClock)
	adopt(s)
	s.Apply(testClock, []byte(`{"_type":"setparam","system_cfg":"outlet.2.relay_state=disabled"}`))
	s.Apply(testClock, []byte(`{"_type":"cmd","cmd":"setdefault"}`))
	adopt(s)

	m := decode(t, s)
	ot := m["outlet_table"].([]any)
	if got := ot[1].(map[string]any)["relay_state"]; got != true {
		t.Errorf("outlet 2 relay_state = %v after factory reset, want the default true", got)
	}
}

func TestAdoptedPayloadUPSReportsBatteryState(t *testing.T) {
	s := NewSession(upsDesc(), testInformURL, testClock)
	adopt(s)
	m := decode(t, s)

	vbms, ok := m["vbms_table"].(map[string]any)
	if !ok {
		t.Fatalf("vbms_table missing on a device claiming the NUT capability: %v", m["vbms_table"])
	}
	// The on-battery signal drives the controller's AC-lost and
	// AC-restored alerts, so an emulated UPS has to report a healthy mains
	// feed rather than leave the key absent.
	if vbms["is_battery_mode"] != false {
		t.Errorf("is_battery_mode = %v, want false", vbms["is_battery_mode"])
	}
	pool, ok := vbms["battpool"].(map[string]any)
	if !ok {
		t.Fatalf("battpool missing: %v", vbms["battpool"])
	}
	// The casing here is the wire's, not ours: the controller matches these
	// names exactly, so a tidied-up battery_level would report nothing.
	for _, k := range []string{"batteryLevel", "timeToRemain", "ischarging", "capWh"} {
		if _, present := pool[k]; !present {
			t.Errorf("battpool missing %q", k)
		}
	}
}

// The NUT bit is the discriminator. A power device without it is a power strip,
// and reporting battery state for one would invent hardware it does not have.
func TestPowerDeviceWithoutNUTCapabilityHasNoBatteryState(t *testing.T) {
	d := upsDesc()
	d.SmartPowerCaps = SmartPowerCapBuzzer
	s := NewSession(d, testInformURL, testClock)
	adopt(s)
	m := decode(t, s)
	if _, present := m["vbms_table"]; present {
		t.Errorf("vbms_table present without the NUT capability bit")
	}
	// smart_power_caps itself still goes out: the buzzer is real.
	if m["smart_power_caps"] != float64(SmartPowerCapBuzzer) {
		t.Errorf("smart_power_caps = %v, want %d", m["smart_power_caps"], SmartPowerCapBuzzer)
	}
}

func TestPowerCommandsProduceEffects(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind EffectKind
		text string
	}{
		{"power cycle carries the port", `{"_type":"cmd","cmd":"power-cycle","port":3}`, EffectPowerCycle, "3"},
		// relayctl names the outlets it acts on in a selection list whose
		// entries carry only an index.
		{"relayctl carries an outlet selection",
			`{"_type":"cmd","cmd":"relayctl","outlet_table":[{"index":1},{"index":3}],"time":"1700000000000"}`,
			EffectRelayCtl, "1,3"},
		// A second form of the command carries no list at all, so an
		// empty selection is valid rather than a malformed reply.
		{"relayctl without a selection is valid", `{"_type":"cmd","cmd":"relayctl"}`, EffectRelayCtl, ""},
		{"rps recovery carries the port", `{"_type":"cmd","cmd":"rps-port-recovery","port":2}`, EffectRPSPortRecovery, "2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession(pduDesc(), testInformURL, testClock)
			adopt(s)
			effects := s.Apply(testClock, []byte(tc.body))
			if len(effects) != 1 {
				t.Fatalf("got %d effects, want 1: %v", len(effects), effects)
			}
			if effects[0].Kind != tc.kind {
				t.Errorf("kind = %v, want %v", effects[0].Kind, tc.kind)
			}
			if effects[0].Text != tc.text {
				t.Errorf("text = %q, want %q", effects[0].Text, tc.text)
			}
		})
	}
}
