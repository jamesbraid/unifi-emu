package inform

// Port is one switch/gateway/ethernet port in a model's layout. The JSON tags
// are the on-the-wire inform names (port_table entries build from them).
type Port struct {
	IfName   string `json:"ifname"`
	Name     string `json:"name"`
	PortIdx  int    `json:"port_idx"`
	Media    string `json:"media"` // "GE", "SFP+"
	PoECaps  int    `json:"poe_caps"`
	IsUplink bool   `json:"is_uplink"`
}

// Radio is one wireless radio in an AP model's layout.
type Radio struct {
	Name        string `json:"name"`  // "wifi-ng", "wifi-na"
	Radio       string `json:"radio"` // "ng", "na"
	Channel     int    `json:"-"`     // set by the caller; not carried on the inform wire
	HT          string `json:"ht"`    // "20", "40"
	MinTxPower  int    `json:"min_txpower"`
	MaxTxPower  int    `json:"max_txpower"`
	NSS         int    `json:"nss"`
	RadioCaps   int    `json:"radio_caps"`
	AntennaGain int    `json:"antenna_gain"`
}

// Outlet is one switchable or metered outlet in a power device's layout. A
// power device's outlets are what its ports are to a switch: the structural
// table the controller renders its UI from. Index is 1-based and matches the
// position the controller addresses in outlet_overrides.
type Outlet struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	Group       string `json:"group,omitempty"` // "standard", "usb", "surge"
	HasRelay    bool   `json:"has_relay"`       // the relay can be switched
	HasMetering bool   `json:"has_metering"`    // reports voltage/current/power
	Caps        int    `json:"outlet_caps,omitempty"`
	// Type is the outlet_type a device using the older encoding reports
	// alongside its capability bits: 1 for a USB outlet, 0 for an AC one.
	// Devices on the newer encoding do not send it.
	Type int `json:"outlet_type,omitempty"`
}

// PSU is one power supply in a redundant-power device's psu_table. Caps is the
// psu_caps bitmap and Type the psu_type power-method/supply descriptor; both
// are per-supply, not per-device.
type PSU struct {
	Index int `json:"index"`
	Caps  int `json:"psu_caps,omitempty"`
	Type  int `json:"psu_type,omitempty"`
}

// Descriptor is the fully-resolved identity and shape of one device: everything
// the inform payload needs about who the device is, none of it mutated during a
// session. The caller resolves all defaults, overrides, and the fw_caps
// placeholder before constructing it; the payload builder reports these values
// verbatim.
type Descriptor struct {
	MAC          string
	Serial       string
	Model        string
	ModelDisplay string
	Version      string
	IP           string
	Hostname     string
	Type         string // "uap" | "usw" | "ugw" | "uxg" | "usp"
	FWCaps       int    // firmware capability bitmap to report
	UDAPIVersion string // "" = no UDAPI config plane
	UDAPICaps    int
	Ports        []Port
	Radios       []Radio
	SSIDs        []string // non-empty opts an AP into emitting vaps
	// Outlets and PSUs are the power-device shape, the way Ports is the
	// switch shape. Both are empty for a device that has neither.
	Outlets []Outlet
	PSUs    []PSU
	// SmartPowerCaps is the device-level power capability bitmap. It gates
	// UPS behaviour in the controller, so a battery-backed device that
	// reports 0 here renders as a plain power strip.
	SmartPowerCaps int
	// HWCaps says what hardware the device has. The controller believes it
	// over anything else the payload claims: a device that reports outlets
	// without declaring the outlet bit here has its outlet table discarded
	// without a word in the log.
	HWCaps int
	// USGCaps is the gateway feature bitmap. The controller offers every
	// claimed feature against the device, so it carries only the two a
	// real gateway on current firmware claims and this one can honour;
	// the same two go out as has_* booleans, which the controller ORs
	// back into the bitmap before storing it. Zero on anything that is
	// not a gateway.
	USGCaps int
}

// Outlet capability bits, as reported per row in outlet_table[].outlet_caps.
// OutletCapAC doubles as the encoding-version test: the controller reads a
// value of at least OutletCapAC as the current encoding and otherwise falls
// back to the legacy outlet_type field.
const (
	OutletCapHasRelay    = 1
	OutletCapPowerMeter  = 2
	OutletCapAutoRelay   = 4
	OutletCapButtonState = 8
	OutletCapAC          = 65536
	OutletCapUSB         = 131072
)

// Power-supply capability bits, as reported per row in psu_table[].psu_caps.
const (
	PSUCapChargeCtrl   = 1
	PSUCapPowerMeter   = 2
	PSUCapHotSwappable = 4
)

// Device-level smart-power capability bits. NUT_INFORMATION_ACCESS is the
// bit that makes the controller treat a device as a UPS rather than a plain
// power strip.
const (
	SmartPowerCapNUTInformationAccess       = 1
	SmartPowerCapAutoPowerCycleOnACRecovery = 2
	SmartPowerCapBuzzer                     = 4
	SmartPowerCapSafeShutdownAndCycleTime   = 8
	SmartPowerCapEmergencyPowerOff          = 16
	SmartPowerCapACInputVoltageTHDTolerance = 64
)

// Hardware capability bits, reported in hw_caps. These describe what the
// device physically has, and the controller gates features on them: the outlet
// bit is what makes it accept an outlet table at all.
const (
	HWCapScreen  = 1
	HWCapLEDBar  = 2
	HWCap5GOnly  = 4
	HWCapLCM     = 8
	HWCapRPS     = 16
	HWCapSpeaker = 32
	HWCapOutlet  = 128
)

// Gateway feature bits, usg_caps. The five low bits double as booleans in
// the same inform -- has_dpi, has_porta, has_default_route_distance,
// has_ssh_disable, and a non-zero radius_caps -- and the controller ORs
// those into the bitmap before it stores it.
const (
	USGCapDPI                  = 1
	USGCapPortA                = 2
	USGCapDefaultRouteDistance = 4
	USGCapSSHDisable           = 8
	USGCapRadius               = 16
)

// PlaceholderFWCaps is what a device reports when no real bitmap was captured
// for its firmware: a single bit nothing reads, so the value claims nothing
// while staying distinct from a measured one and from an absent field.
//
// It used to set bit 0 as well, on the understanding that nothing tested it.
// That is not so. Bit 0 is the SSH capability, and it decides whether the
// controller will try to adopt the device over SSH rather than by inform --
// which an emulated device cannot service. Bit 1 has no reader at all, so the
// placeholder keeps that one and drops the other.
const PlaceholderFWCaps = 2
