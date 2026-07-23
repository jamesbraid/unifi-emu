package emu

import (
	"encoding/json"
	"log"
	"strings"
	"time"
)

// informResponse is the controller's reply to an inform packet. mgmt_cfg
// is a single string of newline-separated k=v pairs, not a JSON object.
type informResponse struct {
	Type       string `json:"_type"`
	Cmd        string `json:"cmd"`
	Key        string `json:"key"`
	URI        string `json:"uri"`
	Interval   int    `json:"interval"`
	MgmtCfg    string `json:"mgmt_cfg"`
	Cfgversion string `json:"cfgversion"`
	Version    string `json:"version"` // upgrade target firmware version
}

// applyResponse applies one decoded controller response to the device.
//
// Key-rotation rule (the OpenUniFi ADOPTION_FIX.md stuck-loop bug this
// project must not repeat): a new authkey can arrive two ways — the
// set-adopt command (unconditional; it is the authority on builds that
// send it), or mgmt_cfg.authkey, which is adopted only while the device
// is still on the default key and is the ONLY channel on controller
// builds that never send set-adopt (verified live against the -sim
// image: its mgmt_cfg authkey equals the device doc's x_authkey). Once
// the device holds a real key, later mgmt_cfg authkeys are controller
// bookkeeping and must not clobber it — saving them is what strands a
// device in an adopt loop.
func (d *device) applyResponse(body []byte) {
	var r informResponse
	if err := json.Unmarshal(body, &r); err != nil {
		log.Printf("%s: ignoring undecodable response: %v", d.spec.MAC, err)
		return
	}
	switch r.Type {
	case "cmd":
		d.applyCmd(r)
	case "setparam":
		d.applySetparam(r)
	case "setstate":
		d.applySetstate(body, r.Cfgversion)
	case "noop":
		if r.Interval > 0 {
			d.mu.Lock()
			d.interval = time.Duration(r.Interval) * time.Second
			d.mu.Unlock()
		}
	case "upgrade":
		// Real firmware downloads, flashes and reboots into the new
		// version; the controller holds the device in state=4 (upgrading)
		// until an inform arrives carrying that version. Emulate the
		// reboot: adopt the target version and restart uptime, so the
		// next inform completes the "upgrade". Verified live against the
		// -sim image: its upgrade cmd for U7PRO carried
		// {"version":"8.6.11.18870","url":"https://fw-download...bin"}.
		d.mu.Lock()
		upgraded := r.Version != ""
		if upgraded {
			d.spec.Version = r.Version
		}
		d.started = time.Now()
		d.mu.Unlock()
		if upgraded {
			log.Printf("%s: upgrade to %s applied (emulated reboot)", d.spec.MAC, r.Version)
		} else {
			log.Printf("%s: upgrade requested without a target version (emulated reboot)", d.spec.MAC)
		}
	default:
		log.Printf("%s: ignoring unknown response _type %q", d.spec.MAC, r.Type)
	}
}

func (d *device) applyCmd(r informResponse) {
	switch r.Cmd {
	case "set-adopt", "adopt":
		d.mu.Lock()
		if r.Key != "" {
			d.key = r.Key
		}
		if r.URI != "" {
			d.informURL = r.URI
		}
		d.adopted = true
		d.state = StateAdopting
		d.mu.Unlock()
		log.Printf("%s: set-adopt received, key rotated, informing %s, now ADOPTING",
			d.spec.MAC, r.URI)
	case "setdefault":
		d.mu.Lock()
		d.adopted = false
		d.key = DefaultKey
		d.cfgvers = "0"
		d.state = StatePending
		d.setstate = nil
		d.mu.Unlock()
		log.Printf("%s: setdefault received, factory reset, back to PENDING", d.spec.MAC)
	default:
		log.Printf("%s: ignoring cmd %q", d.spec.MAC, r.Cmd)
	}
}

func (d *device) applySetparam(r informResponse) {
	// mgmt_cfg can arrive on every inform; log it only when it changes so
	// the provisioning handshake stays visible without per-inform spam.
	d.mu.Lock()
	changed := r.MgmtCfg != d.lastMgmt
	d.lastMgmt = r.MgmtCfg
	d.mu.Unlock()
	if changed {
		log.Printf("%s: mgmt_cfg: %q", d.spec.MAC, r.MgmtCfg)
	}

	var cfgvers, authkey string
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
		}
	}

	d.mu.Lock()
	if cfgvers != "" {
		d.cfgvers = cfgvers
	}
	// Rotate to the mgmt_cfg authkey only while still on the default key
	// — see the key-rotation rule on applyResponse.
	rotated := false
	if authkey != "" && authkey != DefaultKey && d.key == DefaultKey {
		d.key = authkey
		d.adopted = true
		d.state = StateAdopting
		rotated = true
	}
	d.mu.Unlock()
	if rotated {
		log.Printf("%s: authkey adopted from mgmt_cfg, now ADOPTING", d.spec.MAC)
	}
}

// applySetstate records the cfgversion and stashes the raw provisioned
// tables so later payloads echo them back to the controller.
func (d *device) applySetstate(body []byte, cfgversion string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		log.Printf("%s: ignoring undecodable setstate: %v", d.spec.MAC, err)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if cfgversion != "" {
		d.cfgvers = cfgversion
	}
	if d.setstate == nil {
		d.setstate = map[string]json.RawMessage{}
	}
	for _, k := range []string{"radio_table", "vap_table", "port_table", "port_overrides"} {
		if v, ok := raw[k]; ok {
			d.setstate[k] = v
		}
	}
}
