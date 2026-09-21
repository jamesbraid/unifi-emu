package inform

import (
	"fmt"
	"net"
)

// macHeader parses mac into the 6-byte form used as a device identity and as
// the seed for derived addresses (vap BSSIDs). A MAC that fails to parse
// yields the zero value.
func macHeader(mac string) [6]byte {
	var out [6]byte
	if hw, err := net.ParseMAC(mac); err == nil && len(hw) == 6 {
		copy(out[:], hw)
	}
	return out
}

func portTable(desc Descriptor) []map[string]any {
	ports := desc.Ports
	table := make([]map[string]any, 0, len(ports))
	for _, p := range ports {
		table = append(table, map[string]any{
			"ifname":      p.IfName,
			"name":        p.Name,
			"port_idx":    p.PortIdx,
			"media":       p.Media,
			"poe_caps":    p.PoECaps,
			"is_uplink":   p.IsUplink,
			"up":          true,
			"speed":       1000,
			"full_duplex": true,
			"rx_bytes":    0,
			"tx_bytes":    0,
		})
	}
	return table
}

func ethernetTable(desc Descriptor) []map[string]any {
	return []map[string]any{{
		"mac":      desc.MAC,
		"name":     "eth0",
		"num_port": len(desc.Ports),
	}}
}

func radioTable(desc Descriptor) []map[string]any {
	table := make([]map[string]any, 0, len(desc.Radios))
	for _, r := range desc.Radios {
		table = append(table, map[string]any{
			"name":             r.Name,
			"radio":            r.Radio,
			"channel":          r.Channel,
			"ht":               r.HT,
			"min_txpower":      r.MinTxPower,
			"max_txpower":      r.MaxTxPower,
			"nss":              r.NSS,
			"tx_power":         r.MaxTxPower,
			"radio_caps":       r.RadioCaps,
			"antenna_gain":     r.AntennaGain,
			"builtin_antenna":  true,
			"builtin_ant_gain": r.AntennaGain,
		})
	}
	return table
}

func radioTableStats(desc Descriptor) []map[string]any {
	table := make([]map[string]any, 0, len(desc.Radios))
	for _, r := range desc.Radios {
		table = append(table, map[string]any{
			"name":       r.Name,
			"channel":    r.Channel,
			"tx_power":   r.MaxTxPower,
			"cu_self_tx": 0,
			"cu_self_rx": 0,
			"cu_total":   0,
			"num_sta":    0,
			"noise":      -95,
		})
	}
	return table
}

// vapTable renders the AP's virtual access points. Empty by default:
// this controller build rejects default vaps (their id is not a valid
// wlanconf ObjectId) with ERROR noise on every inform and drops them.
// Vaps appear only when the caller opts in via Descriptor.SSIDs, or when
// the controller provisions real WLAN config via setstate (echoed over
// the defaults by BuildPayload).
func vapTable(desc Descriptor) []map[string]any {
	ssids := desc.SSIDs
	mac := macHeader(desc.MAC)
	table := make([]map[string]any, 0, len(ssids)*len(desc.Radios))
	idx := 0
	for _, r := range desc.Radios {
		for _, ssid := range ssids {
			bssid := mac
			// Locally administered, so BSSIDs never collide with any
			// device's base MAC. Offset the second-to-last octet: adjacent
			// fleet MACs differ in the last octet, so offsetting there
			// collided vap N of one AP with vap 0 of the next.
			bssid[0] |= 0x02
			bssid[4] += byte(idx)
			table = append(table, map[string]any{
				"essid":      ssid,
				"bssid":      net.HardwareAddr(bssid[:]).String(),
				"name":       fmt.Sprintf("wlan%d", idx),
				"radio":      r.Radio,
				"up":         true,
				"channel":    r.Channel,
				"tx_power":   r.MaxTxPower,
				"num_sta":    0,
				"usage":      "user",
				"id":         "user",
				"ccq":        0,
				"rx_bytes":   0,
				"tx_bytes":   0,
				"rx_packets": 0,
				"tx_packets": 0,
				"sta_table":  []any{},
			})
			idx++
		}
	}
	return table
}

// outletTable renders a power device's outlets.
//
// Two encodings are in service at once, and a device uses one or the other
// depending on which it is, not on how new its firmware is. Rack PDUs describe
// an outlet with a small outlet_caps value beside an outlet_type, and report
// their measurements as decimal strings. Plugs, strips and the battery-backed
// models describe the same outlet with a capability value at or above the AC
// class bit, a pair of has_relay/has_metering booleans, no outlet_type, and
// numeric measurements. Sending one family's shape for the other leaves fields
// the controller reads for that model absent.
//
// The capability value chooses between them, because it is what the controller
// itself tests: a value at or above the AC class bit is the newer encoding, and
// anything smaller sends it looking for outlet_type.
//
// Either way, an outlet that does not meter omits the measurement keys rather
// than reporting zeros -- a real device leaves them out, and a zero reads as a
// real measurement of no load.
func outletTable(desc Descriptor) []map[string]any {
	table := make([]map[string]any, 0, len(desc.Outlets))
	for _, o := range desc.Outlets {
		// Neither name nor cycle_enabled is reported. Both belong to the
		// controller, not the device: it holds them in the outlet
		// overrides and copies them onto the entry it stores. It also
		// only stores the table when that copy changed something, so a
		// device that reports the operator's own name and cycle policy
		// back at it reports nothing new, and the table is dropped
		// without a word. Leaving them out is both what real hardware
		// does and what makes the table persist.
		entry := map[string]any{
			"index":       o.Index,
			"relay_state": true,
			"outlet_caps": o.Caps,
		}
		if o.Caps >= OutletCapAC {
			entry["has_relay"] = o.HasRelay
			entry["has_metering"] = o.HasMetering
			if o.HasMetering {
				// Static, like every other synthetic counter the
				// emulator reports: a fixed sample is reproducible
				// across informs, which is what the time series needs.
				entry["outlet_voltage"] = 120.0
				entry["outlet_current"] = 0.0
				entry["outlet_power"] = 0.0
			}
			table = append(table, entry)
			continue
		}

		entry["outlet_type"] = o.Type
		// Per-outlet state a rack PDU reports on every outlet, metered or
		// not. The emulator has no faults to report and nothing pending,
		// so these are the quiet values.
		entry["power_fault"] = false
		entry["power_warning"] = false
		entry["relay_activation_countdown"] = 0
		entry["relay_activation_time"] = 0
		entry["modem_power_cycle_count"] = 0
		if o.Group == "usb" {
			// The USB outlets share a relay, and only they carry the
			// group they share it through.
			entry["relay_group"] = 1
		}
		if o.HasMetering {
			// Decimal strings, which is what this family sends. The
			// controller parses them; a bare number is the other
			// family's spelling.
			entry["outlet_voltage"] = "120.000"
			entry["outlet_current"] = "0.000"
			entry["outlet_power"] = "0.000"
			entry["outlet_power_factor"] = "0.000"
		}
		table = append(table, entry)
	}
	return table
}

// psuTable renders a redundant-power device's supplies. psu_caps and psu_type
// are per-supply: two supplies in one device can differ in both.
func psuTable(desc Descriptor) []map[string]any {
	table := make([]map[string]any, 0, len(desc.PSUs))
	for _, p := range desc.PSUs {
		table = append(table, map[string]any{
			"index":    p.Index,
			"psu_caps": p.Caps,
			"psu_type": p.Type,
		})
	}
	return table
}

// ifTable renders the device's interfaces. It is the join target for uplink,
// which carries an interface *name*: the controller looks the name up here and
// builds its own uplink record from the matching entry. A device that reports
// no if_table has nothing for uplink to resolve against.
//
// Counters are static, like port_table's. time_delta is reported so the
// controller derives the rate fields itself rather than reading zeros.
func ifTable(desc Descriptor) []map[string]any {
	table := make([]map[string]any, 0, len(desc.Ports))
	for _, p := range desc.Ports {
		entry := map[string]any{
			"name":        p.IfName,
			"ip":          desc.IP,
			"netmask":     "255.255.255.0",
			"mac":         desc.MAC,
			"up":          true,
			"enable":      true,
			"speed":       mediaSpeed(p.Media),
			"full_duplex": true,
			"num_port":    1,
			"time_delta":  10.0,
			"rx_bytes":    0,
			"tx_bytes":    0,
			"rx_packets":  0,
			"tx_packets":  0,
			"rx_dropped":  0,
			"tx_dropped":  0,
			"rx_errors":   0,
			"tx_errors":   0,
		}
		// The uplink interface is the one the controller resolves the
		// default route through, so it is the only one that carries
		// learned DNS and gateway lists.
		if p.IsUplink {
			entry["nameservers"] = []string{"127.0.0.53"}
			entry["gateways"] = []string{"127.0.0.1"}
		}
		table = append(table, entry)
	}
	return table
}

// uplinkName is the interface the device reports as its uplink: the name the
// controller looks up in if_table. It is the port flagged as the uplink in the
// model layout, falling back to the first interface, and to eth0 for a device
// with no ports at all.
func uplinkName(desc Descriptor) string {
	for _, p := range desc.Ports {
		if p.IsUplink {
			return p.IfName
		}
	}
	if len(desc.Ports) > 0 {
		return desc.Ports[0].IfName
	}
	return "eth0"
}

// mediaSpeed maps a model profile's media string to the negotiated speed the
// interface reports. The controller reads speed with an integer accessor, so
// these stay whole numbers.
func mediaSpeed(media string) int {
	switch media {
	case "10GbE", "SFP+":
		return 10000
	case "SFP28":
		return 25000
	case "QSFP28":
		return 100000
	case "2.5GbE":
		return 2500
	case "FE":
		// The power lineup's management interface is a 100 Mb port, so a
		// gigabit default would overstate every PDU and UPS.
		return 100
	default:
		return 1000
	}
}

// vbmsTable renders a battery-backed device's battery-management state. It is
// an object, not a table, despite the name the wire uses for it.
//
// The key casing inside it is inconsistent -- batteryLevel and capWh sit beside
// batt_available_cnt and ischarging. That is the wire's spelling, not a typo:
// the controller matches these names exactly, so normalizing them would make
// the device report nothing.
func vbmsTable(desc Descriptor) map[string]any {
	return map[string]any{
		// The on-battery signal. False means mains present, which is
		// what drives the controller's AC-restored and battery-in-use
		// alerts, so an emulated UPS reports a healthy mains feed.
		"is_battery_mode": false,
		"bms_run_anomaly": 0,
		"epo_enabled":     desc.SmartPowerCaps&SmartPowerCapEmergencyPowerOff != 0,
		"input_thd_level": 0,
		"battpool": map[string]any{
			"batteryLevel":              100,
			"batteryLevelLow":           20,
			"batteryLevelLowest":        10,
			"timeToRemain":              3600,
			"ischarging":                false,
			"capWh":                     100,
			"batt_available_cnt":        1,
			"batt_available_power":      100,
			"batt_total_power":          100,
			"readycnt":                  1,
			"device_total_power_budget": 100,
			"device_total_power_output": 0,
			// Output and bypass measurements. These are plain integers,
			// not the millivolt/milliamp scaling battery_table uses, so
			// they are reported unscaled.
			"device_output_voltage": 120,
			"device_output_current": 0,
			"device_bypass_voltage": 120,
		},
		"battery_table": []map[string]any{{
			"id":             desc.Serial,
			"model":          desc.Model,
			"fwv":            desc.Version,
			"batteryHealth":  100,
			"batteryAnomaly": 0,
			"batteryMV":      12000,
			"batteryMA":      0,
			"health":         "Good",
			"isBadBattery":   false,
			"uptime":         0,
			"isLocating":     false,
			"client_state":   "ready",
			"upgradable":     false,
		}},
	}
}
