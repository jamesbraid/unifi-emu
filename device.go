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
type DeviceSpec struct {
	MAC          string
	Type         string
	Model        string
	ModelDisplay string
	Version      string
	Name         string
	IP           string
	Ports        int      // overrides the profile port layout when > 0
	SSIDs        []string // overrides the profile default vaps when non-empty
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
