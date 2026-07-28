// Package emu emulates UniFi devices (UAP/USW/UGW) against a real
// UniFi controller using the inform protocol.
package emu

import (
	"encoding/json"
	"fmt"
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
	MAC          string `json:"mac" yaml:"mac"`
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
	Serial string `json:"serial" yaml:"serial"`
}

// deviceSpecKeys is the set of accepted fleet-file keys. A mapping entry
// with any other key is rejected, preserving the strictness the JSON loader
// had via DisallowUnknownFields (a custom UnmarshalYAML bypasses the outer
// decoder's KnownFields, so the check lives here).
var deviceSpecKeys = map[string]bool{
	"mac": true, "type": true, "model": true, "modeldisplay": true,
	"version": true, "name": true, "ip": true, "ports": true, "ssids": true,
	"fwcaps": true, "serial": true,
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

// device is the mutable runtime state of one emulated device.
type device struct {
	spec    DeviceSpec
	profile ModelProfile
	started time.Time

	mu        sync.Mutex
	state     DeviceState
	adopted   bool
	key       string
	informURL string
	cfgvers   string
	useAESGCM bool
	interval  time.Duration
	setstate  map[string]json.RawMessage

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
		spec:      spec,
		profile:   profile,
		started:   time.Now(),
		state:     StatePending,
		key:       DefaultKey,
		informURL: informURL,
		cfgvers:   "0",
		interval:  10 * time.Second,
	}, nil
}

// macHeader parses spec.MAC into the 6-byte form the inform header wants.
func (d *device) macHeader() [6]byte {
	var mac [6]byte
	hw, err := net.ParseMAC(d.spec.MAC)
	if err != nil {
		return mac
	}
	copy(mac[:], hw)
	return mac
}

// NextMAC returns baseMAC advanced by n as a 48-bit integer.
func NextMAC(base net.HardwareAddr, n int) string {
	v := uint64(0)
	for _, b := range base {
		v = v<<8 | uint64(b)
	}
	v += uint64(n)
	var mac [6]byte
	for i := 5; i >= 0; i-- {
		mac[i] = byte(v)
		v >>= 8
	}
	return net.HardwareAddr(mac[:]).String()
}

// NextIP returns baseIP's last octet advanced by n, erroring if it leaves
// the /24.
func NextIP(base net.IP, n int) (string, error) {
	last := int(base[3]) + n
	if last > 254 {
		return "", fmt.Errorf("auto-IP left the base /24 (%s + %d > .254); set SIM_IP_BASE or give explicit ips", base, n)
	}
	ip := net.IPv4(base[0], base[1], base[2], byte(last))
	return ip.String(), nil
}

// ExpandFleet fills omitted mac/ip on an ordered fleet deterministically
// from the bases (mac = base+index+1, ip = base+index), so a restart
// reproduces the same addresses. Explicit spec values always win. It fails
// fast on an unknown model, a fleet with more than one gateway
// (api.err.NoSecondGateway), or an ip that would leave the base /24.
func ExpandFleet(specs []DeviceSpec, macBase, ipBase string) ([]DeviceSpec, error) {
	baseMAC, err := net.ParseMAC(macBase)
	if err != nil {
		return nil, fmt.Errorf("SIM_MAC_BASE %q: %w", macBase, err)
	}
	baseIP := net.ParseIP(ipBase).To4()
	if baseIP == nil {
		return nil, fmt.Errorf("SIM_IP_BASE %q: not an IPv4 address", ipBase)
	}
	out := make([]DeviceSpec, len(specs))
	gateways := 0
	for i, s := range specs {
		profile, ok := Profile(s.Model)
		if !ok {
			return nil, fmt.Errorf("unknown model %q", s.Model)
		}
		// ugw and uxg are both gateway families and a site adopts one of
		// either, so they share the single slot.
		if profile.Type == "ugw" || profile.Type == "uxg" {
			gateways++
			if gateways > 1 {
				return nil, fmt.Errorf("fleet has %d gateways; a site adopts at most one (api.err.NoSecondGateway)", gateways)
			}
		}
		if s.MAC == "" {
			s.MAC = NextMAC(baseMAC, i+1)
		}
		if s.IP == "" {
			ip, err := NextIP(baseIP, i)
			if err != nil {
				return nil, err
			}
			s.IP = ip
		}
		out[i] = s
	}
	return out, nil
}
