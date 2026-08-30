package emu

import "github.com/jamesbraid/unifi-emu/inform"

// PortSpec and RadioSpec are the emulator's public names for the model-shape
// types, now owned by the inform package so the protocol travels with them.
type PortSpec = inform.Port
type RadioSpec = inform.Radio

// ModelProfile is the per-model shape the controller expects to see:
// identity strings plus the port/radio/SSID layout tables are built from.
type ModelProfile struct {
	Model        string `json:"model"`
	ModelDisplay string `json:"model_display"`
	Type         string `json:"type"` // "ugw", "uxg", "usw", "uap"
	Version      string `json:"version"`
	// UDAPIVersion and UDAPICaps describe the UDAPI config plane, and a
	// model either has both or neither: a device reporting the bitmap
	// with no version has its whole capability update dropped by the
	// controller. Set only for models Ubiquiti documents as having the
	// capability, so most profiles carry neither.
	UDAPIVersion string `json:"udapi_version,omitempty"`
	UDAPICaps    int    `json:"udapi_caps,omitempty"`
	// FWCaps is the firmware capability bitmap, captured from real
	// hardware on this model's firmware. Unset for a firmware nobody has
	// captured, which leaves the device on the built-in placeholder --
	// the controller reads an absent bitmap as 0 and the placeholder sets
	// only bits it never tests, so the two are equivalent to it.
	FWCaps int         `json:"fw_caps,omitempty"`
	Ports  []PortSpec  `json:"ports"`  // usw + ugw + uxg + uap (eth port)
	Radios []RadioSpec `json:"radios"` // uap only
}

// modelRegistry is loaded from the embedded model_profiles.json. That
// fixture is reduced from the controller's stat/device identity dump plus
// the hardware database embedded in its UI bundle; cmd/modelgen validates
// both sources and regenerates the JSON manually (see registry.go for the
// runtime parse step, including radio channel derivation).
var modelRegistry = loadRegistry()
