package unifiemu

import "fmt"

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
	SSIDs        []string    // uap default vaps
}

// switchPorts lays out geCount GE ports followed by sfpCount SFP+ ports,
// port 1 being the uplink (the convention in the oracle captures).
func switchPorts(geCount, sfpCount, poeCaps int) []PortSpec {
	ports := make([]PortSpec, 0, geCount+sfpCount)
	for i := 1; i <= geCount; i++ {
		ports = append(ports, PortSpec{
			IfName:   fmt.Sprintf("eth%d", i-1),
			Name:     fmt.Sprintf("Port %d", i),
			PortIdx:  i,
			Media:    "GE",
			PoECaps:  poeCaps,
			IsUplink: i == 1,
		})
	}
	for i := geCount + 1; i <= geCount+sfpCount; i++ {
		ports = append(ports, PortSpec{
			IfName:  fmt.Sprintf("eth%d", i-1),
			Name:    fmt.Sprintf("Port %d", i),
			PortIdx: i,
			Media:   "SFP+",
		})
	}
	return ports
}

func uapRadios() []RadioSpec {
	return []RadioSpec{
		{Name: "wifi-ng", Radio: "ng", Channel: 1, HT: "20", MinTxPower: 5, MaxTxPower: 24, NSS: 2},
		{Name: "wifi-na", Radio: "na", Channel: 36, HT: "40", MinTxPower: 5, MaxTxPower: 22, NSS: 2},
	}
}

func uapPorts() []PortSpec {
	return []PortSpec{
		{IfName: "eth0", Name: "eth0", PortIdx: 1, Media: "GE", IsUplink: true},
	}
}

// modelRegistry holds the models this controller build knows, version-matched
// to the oracle captures in tmp/. Keyed by model ID.
var modelRegistry = map[string]ModelProfile{
	"UGW3": {
		Model: "UGW3", ModelDisplay: "UniFi Gateway 3P", Type: "ugw",
		Version: "4.4.36.5146617",
		Ports: []PortSpec{
			{IfName: "eth0", Name: "wan", PortIdx: 1, Media: "GE", IsUplink: true},
			{IfName: "eth1", Name: "lan", PortIdx: 2, Media: "GE"},
		},
	},
	"USWED74": {
		Model: "USWED74", ModelDisplay: "UniFi Switch Edge 74", Type: "usw",
		Version: "4.0.21.9965",
		Ports:   switchPorts(24, 2, 0),
	},
	"USM8P": {
		Model: "USM8P", ModelDisplay: "UniFi Switch 8 PoE", Type: "usw",
		Version: "4.0.21.9965",
		Ports:   switchPorts(8, 0, 7),
	},
	"US48P750": {
		Model: "US48P750", ModelDisplay: "UniFi Switch 48 PoE", Type: "usw",
		Version: "4.0.21.9965",
		Ports:   switchPorts(24, 2, 7),
	},
	"USWED06": {
		Model: "USWED06", ModelDisplay: "UniFi Switch Edge 6", Type: "usw",
		Version: "4.0.21.9965",
		Ports:   switchPorts(8, 0, 0),
	},
	"USWF07D": {
		Model: "USWF07D", ModelDisplay: "UniFi Switch Flex 7", Type: "usw",
		Version: "4.0.21.9965",
		Ports:   switchPorts(16, 0, 0),
	},
	"U7MP": {
		Model: "U7MP", ModelDisplay: "UniFi AP 7 Mesh Pro", Type: "uap",
		Version: "4.0.21.9965",
		Ports:   uapPorts(),
		Radios:  uapRadios(),
		SSIDs:   []string{"UBNT"},
	},
	"U7PRO": {
		Model: "U7PRO", ModelDisplay: "UniFi AP 7 Pro", Type: "uap",
		Version: "4.0.21.9965",
		Ports:   uapPorts(),
		Radios:  uapRadios(),
		SSIDs:   []string{"UBNT"},
	},
	"UAPA6B0": {
		Model: "UAPA6B0", ModelDisplay: "UniFi AP 6 B0", Type: "uap",
		Version: "4.0.21.9965",
		Ports:   uapPorts(),
		Radios:  uapRadios(),
		SSIDs:   []string{"UBNT"},
	},
}
