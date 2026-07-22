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
	interval  time.Duration
	setstate  map[string]json.RawMessage

	// Inform HTTP-status tracking for transition logging: lastStatus is
	// the previous inform's status (0 = none yet), statusRun the count of
	// consecutive informs answered with it.
	lastStatus int
	statusRun  int

	// lastMgmt is the previous mgmt_cfg body, so repeats are not relogged.
	lastMgmt string
}

func newDevice(spec DeviceSpec, informURL string) (*device, error) {
	profile, ok := modelRegistry[spec.Model]
	if !ok {
		return nil, fmt.Errorf("unknown model %q", spec.Model)
	}
	if _, err := net.ParseMAC(spec.MAC); err != nil {
		return nil, fmt.Errorf("bad MAC %q: %w", spec.MAC, err)
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
