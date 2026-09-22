package inform

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	// outletRelay holds relay states the controller pushed as system_cfg
	// lines, keyed by 1-based outlet index. Outlet switching is
	// configuration, not a command: the controller writes the desired
	// state into the device config and expects to read it back in the
	// next inform's outlet_table.
	outletRelay map[int]bool
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
	s.macHeader = macHeader(desc.MAC)
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
	// matrix, carried by the caller in Descriptor.UDAPIVersion/UDAPICaps,
	// not a property of the type: of the gateways that adopt by inform only
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
			// Whole numbers: these three are read with an integer
			// accessor that parses a decimal integer, so "1.5"
			// reads as 0 and the gateway looks idle.
			m["system-stats"] = map[string]any{
				"cpu": "2", "mem": "50", "uptime": strconv.FormatInt(uptime, 10),
			}
			// Mandatory. With config_network_wan absent the
			// controller logs the missing key and skips WAN
			// processing entirely, so the WAN never bootstraps.
			m["config_network_wan"] = map[string]any{"type": "dhcp"}
			m["netmask"] = "255.255.255.0"
			m["if_table"] = ifTable(s.desc)
			// uplink is the *name* of an interface in if_table, not
			// an object: the controller looks the name up and builds
			// its own uplink record from the entry it finds. An
			// object here is silently discarded, and the device ends
			// up with no uplink at all.
			m["uplink"] = uplinkName(s.desc)
		case "usw", "usp":
			// usp is the battery-backed power devices. They report the
			// same wired shape a switch does -- a management port and
			// its ethernet entry -- and carry their outlets and battery
			// state on top, below.
			m["port_table"] = portTable(s.desc)
			m["ethernet_table"] = ethernetTable(s.desc)
			// Same shape as the gateway uplink: the name of an interface
			// in if_table, which the controller resolves to build the
			// device's parent and uplink record. Without it the device
			// adopts and reports fine but hangs off nothing, showing no
			// uplink and no parent.
			m["if_table"] = ifTable(s.desc)
			m["uplink"] = uplinkName(s.desc)
		case "uap":
			m["radio_table"] = radioTable(s.desc)
			m["radio_table_stats"] = radioTableStats(s.desc)
			m["vap_table"] = vapTable(s.desc)
			m["ethernet_table"] = ethernetTable(s.desc)
			m["port_table"] = portTable(s.desc)
		}
		// Power devices are not one device type: rack PDUs and RPS
		// units report as switches, smart plugs as access points, and
		// only the newer battery-backed models have a type of their
		// own. So the power tables hang off what the device actually
		// has, not off its type.
		if len(s.desc.Outlets) > 0 {
			m["outlet_table"] = s.outletTableWithOverrides()
			m["outlet_enabled"] = true
		}
		// What the device physically has. The outlet bit is load-bearing:
		// without it the controller discards the outlet table above and
		// logs nothing, so the device adopts, reports its outlets on every
		// inform, and shows none of them.
		if s.desc.HWCaps != 0 {
			m["hw_caps"] = s.desc.HWCaps
		}
		if len(s.desc.PSUs) > 0 {
			m["psu_table"] = psuTable(s.desc)
		}
		if s.desc.SmartPowerCaps != 0 {
			m["smart_power_caps"] = s.desc.SmartPowerCaps
		}
		// The bit that makes the controller treat the device as a UPS
		// rather than a plain power strip.
		if s.desc.SmartPowerCaps&SmartPowerCapNUTInformationAccess != 0 {
			m["vbms_table"] = vbmsTable(s.desc)
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

// informResponse is the controller's reply to an inform. mgmt_cfg is a single
// string of newline-separated k=v pairs, not a JSON object.
type informResponse struct {
	Type      string `json:"_type"`
	Cmd       string `json:"cmd"`
	Key       string `json:"key"`
	URI       string `json:"uri"`
	Interval  int    `json:"interval"`
	MgmtCfg   string `json:"mgmt_cfg"`
	SystemCfg string `json:"system_cfg"` // device config file, one key=value per line
	// Command arguments. Port is the target of power-cycle and
	// rps-port-recovery; SourceInterface is the speed-test source, which
	// the controller sends in the literal "if!<ifname>" form.
	Port            int    `json:"port"`
	UnitID          int    `json:"unit_id"`
	SourceInterface string `json:"source_interface"`
	// OutletTable is relayctl's selection list: the controller names the
	// outlets to act on, carrying only their index. One variant of the
	// command omits it entirely, so an absent list is valid and means the
	// device applies the command to itself.
	OutletTable []struct {
		Index int `json:"index"`
	} `json:"outlet_table"`
	Cfgversion string `json:"cfgversion"`
	Version    string `json:"version"` // upgrade target firmware version
}

// EffectKind names what Apply did, so a runtime can drive its own state and
// logging without re-parsing the reply.
type EffectKind int

const (
	EffectAdoptingViaSetAdopt EffectKind = iota // Text = new inform URL (may be "")
	EffectAdoptingViaMgmtCfg
	EffectFactoryReset
	EffectRebooted
	EffectUpgraded    // Text = target version, "" if none
	EffectInterval    // Interval carries the new inform interval
	EffectMgmtCfg     // Text = raw mgmt_cfg body
	EffectUnknownCmd  // Text = the ignored cmd
	EffectUnknownType // Text = the ignored _type
	EffectDecodeError // Text = the decode error
	// Appended at the end so the existing values never shift.
	EffectOutletState     // Text = "outlet <n> on|off", from a config push
	EffectPowerCycle      // Text = the port being cycled
	EffectRelayCtl        // relay control command, no argument
	EffectRPSPortRecovery // Text = the RPS port being recovered
	EffectSpeedTest       // Text = the source interface, "" if none
)

// Effect is one thing Apply did. Text and Interval carry the kind's payload.
type Effect struct {
	Kind     EffectKind
	Text     string
	Interval time.Duration
}

// Apply advances the session by one controller reply and returns what changed.
// The key-rotation rule: a mgmt_cfg.authkey is adopted only while the device
// still holds the default key; once it holds a real key, later mgmt_cfg
// authkeys are ignored (the classic stuck-adopt-loop bug). A set-adopt command
// is authoritative and rotates unconditionally.
func (s *Session) Apply(now time.Time, body []byte) []Effect {
	var r informResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return []Effect{{Kind: EffectDecodeError, Text: err.Error()}}
	}
	switch r.Type {
	case "cmd":
		return s.applyCmd(now, r)
	case "setparam":
		return s.applySetparam(r)
	case "setstate":
		return s.applySetstate(body, r.Cfgversion)
	case "noop":
		if r.Interval > 0 {
			return []Effect{{Kind: EffectInterval, Interval: time.Duration(r.Interval) * time.Second}}
		}
		return nil
	case "upgrade":
		// Emulate a flash-and-reboot: adopt the target version and restart
		// uptime, so the next inform completes the controller-held upgrade.
		version := r.Version
		if version != "" {
			s.desc.Version = version
		}
		s.bootTime = now
		return []Effect{{Kind: EffectUpgraded, Text: version}}
	default:
		return []Effect{{Kind: EffectUnknownType, Text: r.Type}}
	}
}

func (s *Session) applyCmd(now time.Time, r informResponse) []Effect {
	switch r.Cmd {
	case "set-adopt", "adopt":
		if r.Key != "" {
			s.key = r.Key
		}
		if r.URI != "" {
			s.informURL = r.URI
		}
		s.adopted = true
		return []Effect{{Kind: EffectAdoptingViaSetAdopt, Text: r.URI}}
	case "setdefault":
		s.adopted = false
		s.key = DefaultKey
		s.cfgversion = "0"
		s.useAESGCM = false
		s.setstate = nil
		s.outletRelay = nil
		return []Effect{{Kind: EffectFactoryReset}}
	case "reboot":
		s.bootTime = now
		return []Effect{{Kind: EffectRebooted}}
	case "power-cycle":
		// Sent for a PoE port that is powering a device, and for a
		// switched outlet on a power device. The port comes back as the
		// effect text so a consumer can act on the specific port.
		return []Effect{{Kind: EffectPowerCycle, Text: strconv.Itoa(r.Port)}}
	case "relayctl":
		// The controller picks the outlets to act on and sends them as a
		// selection list carrying nothing but each index. A second form
		// of the command carries no list at all, so an empty selection
		// is valid rather than a malformed reply.
		idx := make([]string, 0, len(r.OutletTable))
		for _, o := range r.OutletTable {
			idx = append(idx, strconv.Itoa(o.Index))
		}
		return []Effect{{Kind: EffectRelayCtl, Text: strings.Join(idx, ",")}}
	case "rps-port-recovery":
		return []Effect{{Kind: EffectRPSPortRecovery, Text: strconv.Itoa(r.Port)}}
	case "speed-test":
		return []Effect{{Kind: EffectSpeedTest, Text: r.SourceInterface}}
	default:
		return []Effect{{Kind: EffectUnknownCmd, Text: r.Cmd}}
	}
}

func (s *Session) applySetparam(r informResponse) []Effect {
	effects := []Effect{{Kind: EffectMgmtCfg, Text: r.MgmtCfg}}

	var cfgvers, authkey, useAESGCM string
	for _, line := range strings.Split(r.MgmtCfg, "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "cfgversion":
			cfgvers = v
		case "authkey":
			authkey = v
		case "use_aes_gcm":
			useAESGCM = v
		}
	}
	if cfgvers != "" {
		s.cfgversion = cfgvers
	}
	// Rotate to the mgmt_cfg authkey only while still on the default key.
	if authkey != "" && authkey != DefaultKey && s.key == DefaultKey {
		s.key = authkey
		s.adopted = true
		effects = append(effects, Effect{Kind: EffectAdoptingViaMgmtCfg})
	}
	if useAESGCM != "" {
		if enabled, err := strconv.ParseBool(useAESGCM); err == nil {
			s.useAESGCM = enabled
		}
	}
	effects = append(effects, s.applySystemCfg(r.SystemCfg)...)
	return effects
}

// applySystemCfg reads the device configuration file the controller pushes
// alongside mgmt_cfg. Only the outlet relay lines are interpreted: they are how
// outlet switching reaches the device, and the controller re-sends the whole
// file on every inform until the device reports the new state back.
func (s *Session) applySystemCfg(cfg string) []Effect {
	if cfg == "" {
		return nil
	}
	var effects []Effect
	for _, line := range strings.Split(cfg, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		// outlet.<n>.relay_state=enabled|disabled
		rest, found := strings.CutPrefix(k, "outlet.")
		if !found {
			continue
		}
		idxStr, field, ok := strings.Cut(rest, ".")
		if !ok || field != "relay_state" {
			continue
		}
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx <= 0 {
			continue
		}
		on := v == "enabled"
		if s.outletRelay == nil {
			s.outletRelay = make(map[int]bool)
		}
		s.outletRelay[idx] = on
		state := "off"
		if on {
			state = "on"
		}
		effects = append(effects, Effect{
			Kind: EffectOutletState,
			Text: fmt.Sprintf("outlet %d %s", idx, state),
		})
	}
	return effects
}

// outletTableWithOverrides reports the outlet layout with any relay state the
// controller pushed applied over the default. Reporting the pushed state back
// is what stops the controller re-sending the same configuration on every
// inform.
func (s *Session) outletTableWithOverrides() []map[string]any {
	table := outletTable(s.desc)
	for _, entry := range table {
		idx, _ := entry["index"].(int)
		if on, ok := s.outletRelay[idx]; ok {
			entry["relay_state"] = on
		}
	}
	return table
}

func (s *Session) applySetstate(body []byte, cfgversion string) []Effect {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return []Effect{{Kind: EffectDecodeError, Text: err.Error()}}
	}
	if cfgversion != "" {
		s.cfgversion = cfgversion
	}
	if s.setstate == nil {
		s.setstate = map[string]json.RawMessage{}
	}
	for _, k := range []string{"radio_table", "vap_table", "port_table", "port_overrides"} {
		if v, ok := raw[k]; ok {
			s.setstate[k] = v
		}
	}
	return nil
}
