package unifiemu

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
}

// applyResponse applies one decoded controller response to the device.
//
// Key-rotation rule (the OpenUniFi ADOPTION_FIX.md stuck-loop bug this
// project must not repeat): the ONLY source of a new authkey is the
// set-adopt command. mgmt_cfg.authkey is controller bookkeeping — the
// managed key, distinct from both the default key and the set-adopt
// key — and saving it strands the device in an adopt loop. It is
// ignored here on purpose; only cfgversion is taken from mgmt_cfg.
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
		log.Printf("%s: upgrade requested (no-op)", d.spec.MAC)
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
	for _, line := range strings.Split(r.MgmtCfg, "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "cfgversion":
			d.mu.Lock()
			d.cfgvers = v
			d.mu.Unlock()
		case "authkey":
			// Ignored on purpose: see the key-rotation rule on applyResponse.
		}
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
