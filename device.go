// Package emu emulates UniFi devices (UAP/USW/UGW) against a real
// UniFi controller using the inform protocol.
package emu

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/jamesbraid/unifi-emu/inform"
	"gopkg.in/yaml.v3"
)

// DefaultKey is the inform authkey of unadopted devices.
const DefaultKey = inform.DefaultKey

// DeviceState is the adoption state of an emulated device.
type DeviceState int

const (
	StatePending DeviceState = iota
	StateAdopting
	StateConnected
)

func (s DeviceState) String() string {
	switch s {
	case StatePending:
		return "PENDING"
	case StateAdopting:
		return "ADOPTING"
	case StateConnected:
		return "CONNECTED"
	}
	return "UNKNOWN"
}

// DeviceSpec describes one emulated device. Type, ModelDisplay and Version
// default from the model profile when empty; Name defaults to "UBNT".
// An explicit Type must equal the profile's: the profile drives the
// payload shape, so a mismatched Type would describe an incoherent
// device and is an error, not an override.
//
// The json/yaml tags are the fleet-file contract (unifi-emu -devices,
// SIM_DEVICES); keep the two families identical so either format names
// the same keys.
type DeviceSpec struct {
	MAC string `json:"mac" yaml:"mac"`
	// Serial is the reported serial number. Empty derives it from the MAC
	// the way the emulator always has. It exists because a caller that
	// allocates identities up front (the device-container herder) has
	// already chosen the serial and has to see the same one come back:
	// a MAC-derived serial would silently disagree with its own records.
	Serial       string `json:"serial" yaml:"serial"`
	Type         string `json:"type" yaml:"type"`
	Model        string `json:"model" yaml:"model"`
	ModelDisplay string `json:"modeldisplay" yaml:"modeldisplay"`
	Version      string `json:"version" yaml:"version"`
	Name         string `json:"name" yaml:"name"`
	IP           string `json:"ip" yaml:"ip"`
	Ports        int    `json:"ports" yaml:"ports"` // overrides the profile port layout when > 0
	// SSIDs opts the AP into emitting vaps. Empty by default: this
	// controller build rejects default vaps with log noise until a
	// setstate provisions real WLAN config (the setstate echo path
	// overlays vap_table), so devices inform with an empty vap_table.
	SSIDs []string `json:"ssids" yaml:"ssids"`
	// FWCaps overrides the firmware capability bitmap. Every device
	// reports the same fw_caps today, and the value is a placeholder: the
	// controller tests 22 distinct bits against fw_caps and not one of
	// them is a bit the default sets, so what ships is a claim to nothing
	// rather than a conservative claim. This exists to measure what a
	// faithful value would change before any is adopted as a default.
	// Nil leaves the built-in alone; 0 reports the key as zero.
	FWCaps *int `json:"fwcaps" yaml:"fwcaps"`
}

// deviceSpecKeys is the set of accepted fleet-file keys. A mapping entry
// with any other key is rejected, preserving the strictness the JSON loader
// had via DisallowUnknownFields (a custom UnmarshalYAML bypasses the outer
// decoder's KnownFields, so the check lives here).
var deviceSpecKeys = map[string]bool{
	"mac": true, "serial": true, "type": true, "model": true,
	"modeldisplay": true, "version": true, "name": true, "ip": true,
	"ports": true, "ssids": true, "fwcaps": true,
}

// UnmarshalYAML lets a fleet-list entry be a bare model string ("U7PRO") or
// a full mapping. A scalar becomes {Model: scalar}; a mapping decodes the
// known keys and rejects any other. JSON files parse through the same YAML
// decoder, so both formats get this behaviour.
func (d *DeviceSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		d.Model = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("device entry: want a model string or mapping, got yaml kind %d", node.Kind)
	}
	for i := 0; i < len(node.Content); i += 2 {
		if key := node.Content[i].Value; !deviceSpecKeys[key] {
			return fmt.Errorf("device entry: unknown key %q", key)
		}
	}
	// Decode into an alias to avoid recursing into this method.
	type raw DeviceSpec
	var r raw
	if err := node.Decode(&r); err != nil {
		return err
	}
	*d = DeviceSpec(r)
	return nil
}

// device is the mutable runtime of one emulated device: a protocol session plus
// the loop/HTTP concerns around it.
type device struct {
	spec    DeviceSpec
	session *inform.Session

	mu       sync.Mutex
	state    DeviceState
	interval time.Duration

	// Inform HTTP-status tracking for transition logging: lastStatus is
	// the previous inform's status (0 = none yet), statusRun the count of
	// consecutive informs answered with it, and lastStatusLog the time the
	// status was last written to the log -- a long run is relogged on the
	// informStatusHeartbeat interval so a stuck device stays visible.
	lastStatus    int
	statusRun     int
	lastStatusLog time.Time

	// lastMgmt is the previous mgmt_cfg body, so repeats are not relogged.
	lastMgmt string
}

func newDevice(spec DeviceSpec, informURL string) (*device, error) {
	profile, ok := modelRegistry[spec.Model]
	if !ok {
		return nil, fmt.Errorf("unknown model %q", spec.Model)
	}
	hw, err := net.ParseMAC(spec.MAC)
	if err != nil {
		return nil, fmt.Errorf("bad MAC %q: %w", spec.MAC, err)
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("bad MAC %q: want a 6-byte address, got %d bytes", spec.MAC, len(hw))
	}
	if spec.Type == "" {
		spec.Type = profile.Type
	} else if spec.Type != profile.Type {
		// An explicit type that contradicts the profile builds an
		// incoherent device: model identity says one thing, the payload
		// tables another. Fail loudly instead of informing nonsense.
		return nil, fmt.Errorf("type %q does not match model %q (profile type %q)",
			spec.Type, spec.Model, profile.Type)
	}
	if spec.ModelDisplay == "" {
		spec.ModelDisplay = profile.ModelDisplay
	}
	if spec.Version == "" {
		spec.Version = profile.Version
	}
	if spec.Name == "" {
		spec.Name = "UBNT"
	}
	return &device{
		spec:     spec,
		session:  inform.NewSession(buildDescriptor(spec, profile), informURL, time.Now()),
		state:    StatePending,
		interval: 10 * time.Second,
	}, nil
}

// applyResponse feeds one controller reply to the session and reflects the
// result in the device's observable state and logs. Logging happens after the
// lock is released, so log I/O never stalls a State reader.
func (d *device) applyResponse(body []byte) {
	d.mu.Lock()
	var logs []string
	for _, e := range d.session.Apply(time.Now(), body) {
		switch e.Kind {
		case inform.EffectAdoptingViaSetAdopt:
			d.state = StateAdopting
			logs = append(logs, fmt.Sprintf("%s: set-adopt received, key rotated, informing %s, now ADOPTING", d.spec.MAC, e.Text))
		case inform.EffectAdoptingViaMgmtCfg:
			d.state = StateAdopting
			logs = append(logs, fmt.Sprintf("%s: authkey adopted from mgmt_cfg, now ADOPTING", d.spec.MAC))
		case inform.EffectFactoryReset:
			d.state = StatePending
			logs = append(logs, fmt.Sprintf("%s: setdefault received, factory reset, back to PENDING", d.spec.MAC))
		case inform.EffectRebooted:
			logs = append(logs, fmt.Sprintf("%s: reboot requested (emulated reboot)", d.spec.MAC))
		case inform.EffectUpgraded:
			if e.Text != "" {
				logs = append(logs, fmt.Sprintf("%s: upgrade to %s applied (emulated reboot)", d.spec.MAC, e.Text))
			} else {
				logs = append(logs, fmt.Sprintf("%s: upgrade requested without a target version (emulated reboot)", d.spec.MAC))
			}
		case inform.EffectInterval:
			d.interval = e.Interval
		case inform.EffectMgmtCfg:
			if e.Text != d.lastMgmt {
				d.lastMgmt = e.Text
				logs = append(logs, fmt.Sprintf("%s: mgmt_cfg: %q", d.spec.MAC, e.Text))
			}
		case inform.EffectUnknownCmd:
			logs = append(logs, fmt.Sprintf("%s: ignoring cmd %q", d.spec.MAC, e.Text))
		case inform.EffectUnknownType:
			logs = append(logs, fmt.Sprintf("%s: ignoring unknown response _type %q", d.spec.MAC, e.Text))
		case inform.EffectDecodeError:
			logs = append(logs, fmt.Sprintf("%s: ignoring undecodable response: %s", d.spec.MAC, e.Text))
		}
	}
	d.mu.Unlock()
	for _, l := range logs {
		log.Print(l)
	}
}
