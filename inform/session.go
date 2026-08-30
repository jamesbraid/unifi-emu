package inform

import (
	"encoding/json"
	"net"
	"strconv"
	"time"
)

// Session is the device-side inform protocol state machine. It holds the
// mutable adoption state and reports the inform payload; it does no I/O, holds
// no lock, and runs no goroutine. A consumer (the emulator, or a C port) drives
// it: build a payload, POST it, feed the reply to Apply, repeat. Not safe for
// concurrent use — the caller serializes access.
type Session struct {
	desc      Descriptor
	macHeader [6]byte

	key        string
	cfgversion string
	adopted    bool
	useAESGCM  bool
	informURL  string
	setstate   map[string]json.RawMessage
	bootTime   time.Time
}

// NewSession starts a device on the default adoption key, pending, informing at
// informURL. now seeds the uptime clock (uptime = later now - bootTime).
func NewSession(desc Descriptor, informURL string, now time.Time) *Session {
	s := &Session{
		desc:       desc,
		key:        DefaultKey,
		cfgversion: "0",
		informURL:  informURL,
		bootTime:   now,
	}
	if hw, err := net.ParseMAC(desc.MAC); err == nil && len(hw) == 6 {
		copy(s.macHeader[:], hw)
	}
	return s
}

func (s *Session) AuthKey() string   { return s.key }
func (s *Session) InformURL() string { return s.informURL }
func (s *Session) Adopted() bool     { return s.adopted }
func (s *Session) UseAESGCM() bool   { return s.useAESGCM }

// EncodeInform builds the current payload and encrypts it in the negotiated
// mode (AES-GCM once the controller enabled it, AES-CBC before).
func (s *Session) EncodeInform(now time.Time) ([]byte, error) {
	pkt := &Packet{MAC: s.macHeader, Payload: s.BuildPayload(now)}
	if s.useAESGCM {
		return pkt.EncodeGCM(s.key)
	}
	return pkt.Encode(s.key)
}

// BuildPayload renders the inform payload for the session's current state: a
// sparse pending shape before adoption, the full per-type shape after, with any
// provisioned config received via setstate merged over the top. now supplies
// uptime and the wall-clock timestamp so the output is deterministic for a
// fixed clock.
func (s *Session) BuildPayload(now time.Time) []byte {
	uptime := int64(now.Sub(s.bootTime).Seconds())
	m := map[string]any{
		"mac":            s.desc.MAC,
		"serial":         s.desc.Serial,
		"model":          s.desc.Model,
		"model_display":  s.desc.ModelDisplay,
		"version":        s.desc.Version,
		"ip":             s.desc.IP,
		"hostname":       s.desc.Hostname,
		"inform_url":     s.informURL,
		"uptime":         uptime,
		"time":           now.Unix(),
		"cfgversion":     s.cfgversion,
		"x_authkey":      s.key,
		"default":        !s.adopted,
		"_default_key":   !s.adopted,
		"state":          1,
		"fw_caps":        s.desc.FWCaps,
		"isolated":       false,
		"locating":       false,
		"selfrun_beacon": true,
	}
	// A device that runs the UDAPI config plane reports its schema version
	// and capability bitmap on every inform, adopted or not. Both keys go
	// out together, from the one condition: for a device on firmware
	// >= 4.1.0 that reports no udapi_version the controller skips its
	// entire capability-update pass ("Skip updating capability for device
	// [..] due to empty udapi_version" in server.log) and stores none of
	// fw_caps, hw_caps, switch_caps or udapi_caps — so a bitmap sent alone
	// is silently dropped and the device looks less capable than before.
	//
	// Which models have it is a per-model fact from Ubiquiti's published
	// matrix, carried in the profile (see model_overrides.json), not a
	// property of the type: of the gateways that adopt by inform only
	// UXG-Enterprise has UDAPI routing, and the USG line has no UDAPI at
	// all. The controller offers every claimed capability against the
	// device, so claiming one it cannot service is the worse lie.
	if s.desc.UDAPIVersion != "" {
		m["udapi_version"] = map[string]any{"version": s.desc.UDAPIVersion}
		m["udapi_caps"] = s.desc.UDAPICaps
	}
	if s.adopted {
		// Device-side state 4 means managed/adopted; it is not the same
		// state enum as stat/device. OpenUniFi sends 4 for every adopted
		// inform, and newer UOS requires it to finish post-upgrade
		// provisioning. The controller's REST document settles at state 1.
		m["state"] = 4
		m["bootrom_version"] = "unknown"
		m["sys_stats"] = map[string]any{
			"cpu": 1.5, "mem_total": 134217728, "mem_used": 67108864, "mem_buffer": 16777216,
		}
		switch s.desc.Type {
		case "ugw", "uxg":
			m["system-stats"] = map[string]any{
				"cpu": "1.5", "mem": "50.0", "uptime": strconv.FormatInt(uptime, 10),
			}
			m["config_network_wan"] = map[string]any{"type": "dhcp"}
			m["netmask"] = "255.255.255.0"
			m["uplink"] = map[string]any{
				"name": "eth0", "num_port": 1,
				"ip": s.desc.IP, "mac": s.desc.MAC,
				"type": "wire", "up": true,
				"speed": 1000, "max_speed": 1000, "full_duplex": true,
				"rx_bytes": 0, "tx_bytes": 0,
			}
		case "usw":
			m["port_table"] = portTable(s.desc)
			m["ethernet_table"] = ethernetTable(s.desc)
		case "uap":
			m["radio_table"] = radioTable(s.desc)
			m["radio_table_stats"] = radioTableStats(s.desc)
			m["vap_table"] = vapTable(s.desc)
			m["ethernet_table"] = ethernetTable(s.desc)
			m["port_table"] = portTable(s.desc)
		}
	}
	// Echo back provisioned config the controller pushed via setstate.
	for k, v := range s.setstate {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil // unreachable: only JSON-safe values above
	}
	return b
}
