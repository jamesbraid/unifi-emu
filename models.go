package emu

// PortSpec is one switch/gateway/ethernet port in a model's layout.
type PortSpec struct {
	IfName   string `json:"ifname"`
	Name     string `json:"name"`
	PortIdx  int    `json:"port_idx"`
	Media    string `json:"media"` // "GE", "SFP+"
	PoECaps  int    `json:"poe_caps"`
	IsUplink bool   `json:"is_uplink"`
}

// RadioSpec is one wireless radio in an AP model's layout.
type RadioSpec struct {
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

// ModelProfile is the per-model shape the controller expects to see:
// identity strings plus the port/radio/SSID layout tables are built from.
type ModelProfile struct {
	Model        string      `json:"model"`
	ModelDisplay string      `json:"model_display"`
	Type         string      `json:"type"` // "ugw", "usw", "uap"
	Version      string      `json:"version"`
	Ports        []PortSpec  `json:"ports"`  // usw + ugw + uap (eth port)
	Radios       []RadioSpec `json:"radios"` // uap only
}

// modelRegistry is loaded from the embedded model_profiles.json. That
// fixture is reduced from the controller's stat/device identity dump plus
// the hardware database embedded in its UI bundle; cmd/modelgen validates
// both sources and regenerates the JSON manually (see registry.go for the
// runtime parse step, including radio channel derivation).
var modelRegistry = loadRegistry()
