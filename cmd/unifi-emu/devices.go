package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/jamesbraid/unifi-emu"
	"gopkg.in/yaml.v3"
)

// singleFlags are the flags that build one device. Combined with a fleet
// source, one of the two definitions would silently drop, so the
// combination is either rejected (a flag fleet source: -devices, -models)
// or the single-device flags lose (an env fleet source: SIM_DEVICES,
// SIM_MODELS).
var singleFlags = []string{"mac", "type", "model", "model-display", "version", "name", "ip"}

// fleetSources holds the five mutually-exclusive fleet definitions; at most
// one may be set. envInline/models/devicesJSON come from
// SIM_DEVICES/SIM_MODELS/UNIFI_EMU_DEVICES_JSON, devicesFile/modelsFlag from
// -devices/-models.
type fleetSources struct {
	devicesFile string // -devices FILE
	envInline   string // SIM_DEVICES
	devicesJSON string // UNIFI_EMU_DEVICES_JSON
	modelsFlag  string // -models
	models      string // SIM_MODELS (or -models; see modelsText)
}

// modelsText returns the terse-list text from -models or SIM_MODELS and
// whether it came from the flag (explicit CLI intent).
func (s fleetSources) modelsText() (text string, fromFlag bool) {
	if s.modelsFlag != "" {
		return s.modelsFlag, true
	}
	return s.models, false
}

// fleetSpecs resolves the device list from the fleet sources, in src
// (mutually exclusive):
//
//	-devices FILE, SIM_DEVICES env, UNIFI_EMU_DEVICES_JSON env, -models
//	flag, or SIM_MODELS env; any one beats single-device flags
//
// set marks the flags explicitly given on the command line. More than one
// fleet source at once is ambiguous and an error, naming the offenders.
// Single-device flags combined with a flag source (-devices/-models) are
// an error; combined with an env source (SIM_DEVICES/SIM_MODELS) they
// lose, and the losing flag names are returned so the caller can log the
// override. A nil spec slice means single-device mode.
func fleetSpecs(src fleetSources, set map[string]bool) ([]emu.DeviceSpec, []string, error) {
	// Count fleet sources off the pre-collapsed fields directly: -models
	// and SIM_MODELS must each be counted when set, even together. Doing
	// this after modelsText() collapses them to a single winner would hide
	// the case where both are set (the winner leaves only one entry).
	active := []string{}
	if src.devicesFile != "" {
		active = append(active, "-devices")
	}
	if strings.TrimSpace(src.envInline) != "" {
		active = append(active, "SIM_DEVICES")
	}
	if strings.TrimSpace(src.devicesJSON) != "" {
		active = append(active, "UNIFI_EMU_DEVICES_JSON")
	}
	if strings.TrimSpace(src.modelsFlag) != "" {
		active = append(active, "-models")
	}
	if strings.TrimSpace(src.models) != "" {
		active = append(active, "SIM_MODELS")
	}
	if len(active) > 1 {
		return nil, nil, fmt.Errorf("ambiguous device sources: %s are all set", strings.Join(active, ", "))
	}
	if len(active) == 0 {
		return nil, nil, nil // single-device mode
	}
	// At most one fleet source is set, so resolving the models winner now
	// is safe: modelsFlag and models are never both non-empty here.
	modelsText, modelsFromFlag := src.modelsText()
	// A flag fleet source cannot combine with single-device flags.
	flagSource := src.devicesFile != "" || (strings.TrimSpace(modelsText) != "" && modelsFromFlag)
	if flagSource {
		for _, f := range singleFlags {
			if set[f] {
				return nil, nil, fmt.Errorf("%s cannot be combined with -%s", active[0], f)
			}
		}
	}
	var specs []emu.DeviceSpec
	var err error
	switch {
	case strings.TrimSpace(modelsText) != "":
		specs, err = parseModelsList(modelsText)
	case strings.TrimSpace(src.devicesJSON) != "":
		specs, err = parseDevicesJSON(src.devicesJSON)
	default:
		specs, err = loadDevices(src.devicesFile, src.envInline)
	}
	if err != nil {
		return nil, nil, err
	}
	// Env source: single-device flags lose and are logged.
	var ignored []string
	if !flagSource {
		for _, f := range singleFlags {
			if set[f] {
				ignored = append(ignored, f)
			}
		}
	}
	return specs, ignored, nil
}

// defaultFleet loads the image's baked-in default fleet named by
// defaultFile, but only when the user selected nothing at all — no fleet
// source (fleetSpecs already returned single-device mode) and no
// single-device flag. Any explicit choice therefore wins without ever
// colliding with the "ambiguous device sources" guard, which is what lets
// `docker run img` boot the baked fleet while `-e SIM_MODELS=...` or
// `-model X` still overrides it. defaultFile is empty outside the image, so
// this is a no-op for a plain CLI build.
func defaultFleet(defaultFile string, set map[string]bool) ([]emu.DeviceSpec, error) {
	if defaultFile == "" {
		return nil, nil
	}
	for _, f := range singleFlags {
		if set[f] {
			return nil, nil
		}
	}
	return loadDevices(defaultFile, "")
}

// loadDevices reads the fleet from filePath or, when that is empty, from
// the SIM_DEVICES env value envInline. Both hold a YAML list of
// DeviceSpec; JSON files keep working because JSON is a YAML subset.
// Neither set returns (nil, nil): the caller falls back to the
// single-device flags.
func loadDevices(filePath, envInline string) ([]emu.DeviceSpec, error) {
	var src string
	var b []byte
	switch {
	case filePath != "":
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filePath, err)
		}
		src, b = filePath, data
	case strings.TrimSpace(envInline) != "":
		src, b = "SIM_DEVICES", []byte(envInline)
	default:
		return nil, nil
	}
	// KnownFields keeps the strictness the JSON-only loader had with
	// DisallowUnknownFields: a misspelled DeviceSpec key (modle) must
	// fail loudly, not silently drop to the profile default.
	var specs []emu.DeviceSpec
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&specs); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", src, err)
	}
	// A second YAML document must not silently vanish.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse %s: multiple YAML documents; want a single device list", src)
		}
		return nil, fmt.Errorf("parse %s: %w", src, err)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("%s: no devices", src)
	}
	return specs, nil
}

// containerDevice is one entry of the device-container fleet contract
// (UNIFI_EMU_DEVICES_JSON). Its four keys are the whole contract: the
// caller allocates the identity and hands it over, so nothing here is
// derived and nothing else is accepted. It is a separate type from
// emu.DeviceSpec on purpose — DeviceSpec keeps growing keys, and this
// contract must not grow with it by accident.
type containerDevice struct {
	Model  string `json:"model"`
	MAC    string `json:"mac"`
	Serial string `json:"serial"`
	Name   string `json:"name"`
}

// parseDevicesJSON parses the UNIFI_EMU_DEVICES_JSON fleet: a JSON array of
// containerDevice. Unknown keys are rejected the way every other fleet
// source rejects them — a misspelled key would otherwise silently produce a
// device with a derived identity that the caller does not recognise.
func parseDevicesJSON(s string) ([]emu.DeviceSpec, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var entries []containerDevice
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("parse UNIFI_EMU_DEVICES_JSON: %w", err)
	}
	// A second value in the stream would silently vanish, same as the
	// second YAML document loadDevices refuses.
	if dec.More() {
		return nil, errors.New("parse UNIFI_EMU_DEVICES_JSON: trailing content after the device array")
	}
	if len(entries) == 0 {
		return nil, errors.New("UNIFI_EMU_DEVICES_JSON: no devices")
	}
	specs := make([]emu.DeviceSpec, len(entries))
	for i, e := range entries {
		specs[i] = emu.DeviceSpec{Model: e.Model, MAC: e.MAC, Serial: e.Serial, Name: e.Name}
	}
	return specs, nil
}

// parseModelsList parses the terse fleet grammar used by -models and
// SIM_MODELS: a comma-separated list of MODEL or MODEL:count items, e.g.
// "U7PRO,USM8P:2,UGW3". Each unit becomes one DeviceSpec carrying only the
// model; MAC/IP are filled later by expandFleet. Unknown models are caught
// downstream by the registry lookup.
func parseModelsList(s string) ([]emu.DeviceSpec, error) {
	var specs []emu.DeviceSpec
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		model, countText, hasCount := strings.Cut(item, ":")
		model = strings.TrimSpace(model)
		if model == "" {
			return nil, fmt.Errorf("models list: empty model in %q", item)
		}
		count := 1
		if hasCount {
			n, err := strconv.Atoi(strings.TrimSpace(countText))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("models list: bad count in %q", item)
			}
			count = n
		}
		for i := 0; i < count; i++ {
			specs = append(specs, emu.DeviceSpec{Model: model})
		}
	}
	if len(specs) == 0 {
		return nil, errors.New("models list: no models")
	}
	return specs, nil
}

// expandFleet fills omitted mac/ip on an ordered fleet deterministically
// from the bases (mac = base+index+1, ip = base+index), so a restart
// reproduces the same addresses. Explicit spec values always win. It fails
// fast on an unknown model, a fleet with more than one gateway
// (api.err.NoSecondGateway), or an ip that would leave the base /24.
func expandFleet(specs []emu.DeviceSpec, macBase, ipBase string) ([]emu.DeviceSpec, error) {
	baseMAC, err := net.ParseMAC(macBase)
	if err != nil {
		return nil, fmt.Errorf("SIM_MAC_BASE %q: %w", macBase, err)
	}
	baseIP := net.ParseIP(ipBase).To4()
	if baseIP == nil {
		return nil, fmt.Errorf("SIM_IP_BASE %q: not an IPv4 address", ipBase)
	}
	out := make([]emu.DeviceSpec, len(specs))
	gateways := 0
	for i, s := range specs {
		profile, ok := emu.Profile(s.Model)
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
			s.MAC = nextMAC(baseMAC, i+1)
		}
		if s.IP == "" {
			ip, err := nextIP(baseIP, i)
			if err != nil {
				return nil, err
			}
			s.IP = ip
		}
		out[i] = s
	}
	return out, nil
}

// nextMAC returns baseMAC advanced by n as a 48-bit integer.
func nextMAC(base net.HardwareAddr, n int) string {
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

// nextIP returns baseIP's last octet advanced by n, erroring if it leaves
// the /24. The reported IP is cosmetic (the controller keys on MAC), so a
// single /24 of headroom is enough; past it the caller sets SIM_IP_BASE.
func nextIP(base net.IP, n int) (string, error) {
	last := int(base[3]) + n
	if last > 254 {
		return "", fmt.Errorf("auto-IP left the base /24 (%s + %d > .254); set SIM_IP_BASE or give explicit ips", base, n)
	}
	ip := net.IPv4(base[0], base[1], base[2], byte(last))
	return ip.String(), nil
}
