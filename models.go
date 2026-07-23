package emu

//go:generate go run ./cmd/modelgen

// PortSpec is one switch/gateway/ethernet port in a model's layout.
type PortSpec struct {
	IfName   string
	Name     string
	PortIdx  int
	Media    string // "GE", "SFP+"
	PoECaps  int
	IsUplink bool
}

// RadioSpec is one wireless radio in an AP model's layout.
type RadioSpec struct {
	Name        string // "wifi-ng", "wifi-na"
	Radio       string // "ng", "na"
	Channel     int
	HT          string // "20", "40"
	MinTxPower  int
	MaxTxPower  int
	NSS         int
	RadioCaps   int
	AntennaGain int
}

// ModelProfile is the per-model shape the controller expects to see:
// identity strings plus the port/radio/SSID layout tables are built from.
type ModelProfile struct {
	Model        string
	ModelDisplay string
	Type         string // "ugw", "usw", "uap"
	Version      string
	Ports        []PortSpec  // usw + ugw + uap (eth port)
	Radios       []RadioSpec // uap only
}

// modelRegistry is generated from model_profiles.json. That fixture is
// reduced from the controller's stat/device identity dump plus the hardware
// database embedded in its UI bundle; cmd/modelgen validates both sources.
var modelRegistry = generatedModelRegistry
