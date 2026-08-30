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
	Channel     int    `json:"-"`     // derived at load time; not in model_profiles.json
	HT          string `json:"ht"`    // "20", "40"
	MinTxPower  int    `json:"min_txpower"`
	MaxTxPower  int    `json:"max_txpower"`
	NSS         int    `json:"nss"`
	RadioCaps   int    `json:"radio_caps"`
	AntennaGain int    `json:"antenna_gain"`
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
	Type         string // "uap" | "usw" | "ugw" | "uxg"
	FWCaps       int    // firmware capability bitmap to report
	UDAPIVersion string // "" = no UDAPI config plane
	UDAPICaps    int
	Ports        []Port
	Radios       []Radio
	SSIDs        []string // non-empty opts an AP into emitting vaps
}

// PlaceholderFWCaps is what a device reports when no real bitmap was captured
// for its firmware. It sets bits 0 and 1, neither of which the controller tests
// against fw_caps, so it is equivalent to reporting 0 while staying distinct
// from a measured value.
const PlaceholderFWCaps = 3
