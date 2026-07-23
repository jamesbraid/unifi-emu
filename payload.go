package emu

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// buildPayload renders the inform payload for the device's current state:
// a sparse pending shape before adoption, the full per-type shape after,
// with any provisioned config received via setstate merged over the top.
func (d *device) buildPayload() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	uptime := int64(time.Since(d.started).Seconds())
	m := map[string]any{
		"mac":            d.spec.MAC,
		"serial":         strings.ToUpper(strings.ReplaceAll(d.spec.MAC, ":", "")),
		"model":          d.spec.Model,
		"model_display":  d.spec.ModelDisplay,
		"version":        d.spec.Version,
		"ip":             d.spec.IP,
		"hostname":       d.spec.Name,
		"inform_url":     d.informURL,
		"uptime":         uptime,
		"time":           time.Now().Unix(),
		"cfgversion":     d.cfgvers,
		"x_authkey":      d.key,
		"default":        !d.adopted,
		"_default_key":   !d.adopted,
		"state":          1,
		"fw_caps":        3,
		"isolated":       false,
		"locating":       false,
		"selfrun_beacon": true,
	}
	if d.adopted {
		// state 2 = adopted/connected. If a controller ever stalls at this
		// point, the OpenUniFi convention is to fall back to state 4; healthy
		// firmware reports 2, so stick with it unless an oracle says otherwise.
		m["state"] = 2
		m["required_version"] = "4.0.0"
		m["bootrom_version"] = "unknown"
		m["sys_stats"] = map[string]any{
			"cpu":        1.5,
			"mem_total":  134217728,
			"mem_used":   67108864,
			"mem_buffer": 16777216,
		}
		switch d.profile.Type {
		case "ugw":
			m["system-stats"] = map[string]any{
				"cpu":    "1.5",
				"mem":    "50.0",
				"uptime": strconv.FormatInt(uptime, 10),
			}
			m["config_network_wan"] = map[string]any{"type": "dhcp"}
			m["netmask"] = "255.255.255.0"
			m["uplink"] = map[string]any{
				"name": "eth0", "num_port": 1,
				"ip": d.spec.IP, "mac": d.spec.MAC,
				"type": "wire", "up": true,
				"speed": 1000, "max_speed": 1000, "full_duplex": true,
				"rx_bytes": 0, "tx_bytes": 0,
			}
		case "usw":
			m["port_table"] = d.portTable()
			m["ethernet_table"] = d.ethernetTable()
		case "uap":
			m["radio_table"] = d.radioTable()
			m["radio_table_stats"] = d.radioTableStats()
			m["vap_table"] = d.vapTable()
			m["ethernet_table"] = d.ethernetTable()
			m["port_table"] = d.portTable()
		}
	}
	// Echo back provisioned config the controller pushed via setstate.
	for k, v := range d.setstate {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil // unreachable: only JSON-safe values above
	}
	return b
}

// ports returns the spec port-count override synthesized in the profile's
// style, or the profile layout when no override is set.
func (d *device) ports() []PortSpec {
	if d.spec.Ports <= 0 {
		return d.profile.Ports
	}
	ports := make([]PortSpec, 0, d.spec.Ports)
	for i := 1; i <= d.spec.Ports; i++ {
		ports = append(ports, PortSpec{
			IfName:   fmt.Sprintf("eth%d", i-1),
			Name:     fmt.Sprintf("Port %d", i),
			PortIdx:  i,
			Media:    "GE",
			IsUplink: i == 1,
		})
	}
	return ports
}

func (d *device) portTable() []map[string]any {
	ports := d.ports()
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

func (d *device) ethernetTable() []map[string]any {
	return []map[string]any{{
		"mac":      d.spec.MAC,
		"name":     "eth0",
		"num_port": len(d.ports()),
	}}
}

func (d *device) radioTable() []map[string]any {
	table := make([]map[string]any, 0, len(d.profile.Radios))
	for _, r := range d.profile.Radios {
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

func (d *device) radioTableStats() []map[string]any {
	table := make([]map[string]any, 0, len(d.profile.Radios))
	for _, r := range d.profile.Radios {
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
// wlanconf ObjectId) with ERROR noise on every inform and drops them —
// the accepted oracle AP (tmp/oracle-uap.json, gitignored live evidence)
// vap_table. Vaps appear only when the caller opts in via
// DeviceSpec.SSIDs, or when the controller provisions real WLAN config
// via setstate (echoed over the defaults by buildPayload).
func (d *device) vapTable() []map[string]any {
	ssids := d.spec.SSIDs
	mac := d.macHeader()
	table := make([]map[string]any, 0, len(ssids)*len(d.profile.Radios))
	idx := 0
	for _, r := range d.profile.Radios {
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
