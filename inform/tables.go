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
