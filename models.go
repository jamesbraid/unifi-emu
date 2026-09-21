package emu

import "github.com/jamesbraid/unifi-emu/inform"

// PortSpec and RadioSpec are the emulator's public names for the model-shape
// types, now owned by the inform package so the protocol travels with them.
type PortSpec = inform.Port
type RadioSpec = inform.Radio
type OutletSpec = inform.Outlet
type PSUSpec = inform.PSU

// ModelProfile is the per-model shape the controller expects to see:
// identity strings plus the port/radio/SSID layout tables are built from.
type ModelProfile struct {
	Model        string `json:"model"`
	ModelDisplay string `json:"model_display"`
	Type         string `json:"type"` // "ugw", "uxg", "usw", "uap", "usp"
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
	// Outlets is the power-device layout: PDUs, plugs, power strips and
	// the battery-backed models. It crosses type boundaries -- outlet
	// bearing models are typed usw, uap, uxg and usp -- so it is keyed off
	// the model, not the type.
	Outlets []OutletSpec `json:"outlets,omitempty"`
	// PSUs is the redundant-power supply layout, for the RPS models.
	PSUs []PSUSpec `json:"psus,omitempty"`
	// SmartPowerCaps gates the controller's UPS and power behaviour for
	// battery-backed models. Zero for everything else.
	SmartPowerCaps int `json:"smart_power_caps,omitempty"`
	// HWCaps describes the hardware the model physically has. The outlet
	// bit is what makes a controller accept an outlet table.
	HWCaps int `json:"hw_caps,omitempty"`
}

// modelRegistry is loaded from the embedded model_profiles.json. That
// fixture is reduced from the controller's stat/device identity dump plus
// the hardware database embedded in its UI bundle; cmd/modelgen validates
// both sources and regenerates the JSON manually (see registry.go for the
// runtime parse step, including radio channel derivation).
var modelRegistry = loadRegistry()
