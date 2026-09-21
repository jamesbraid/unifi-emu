// Command modelgen reduces an adopted UniFi simulation fleet (or a harvested
// controller hardware database bundle) to model_profiles.json, the model
// catalog the emulator embeds at build time.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type catalogFile struct {
	ControllerVersion string          `json:"controller_version"`
	IdentitySource    string          `json:"identity_source"`
	HardwareSource    string          `json:"hardware_source"`
	Models            []catalogModel  `json:"models"`
	ExcludedModels    []excludedModel `json:"excluded_models,omitempty"`
}

type catalogModel struct {
	Model        string         `json:"model"`
	ModelDisplay string         `json:"model_display"`
	Type         string         `json:"type"`
	Version      string         `json:"version"`
	UDAPIVersion string         `json:"udapi_version,omitempty"`
	UDAPICaps    int            `json:"udapi_caps,omitempty"`
	FWCaps       int            `json:"fw_caps,omitempty"`
	Ports        []catalogPort  `json:"ports,omitempty"`
	Radios       []catalogRadio `json:"radios,omitempty"`
	// Outlets is the power-device layout. It crosses type boundaries --
	// rack PDUs report as switches, smart plugs as access points, and only
	// the newer battery-backed models have a type of their own -- so it is
	// derived from the model's own outlet map rather than from its type.
	Outlets        []catalogOutlet `json:"outlets,omitempty"`
	SmartPowerCaps int             `json:"smart_power_caps,omitempty"`
	HWCaps         int             `json:"hw_caps,omitempty"`
}

type catalogOutlet struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	Group       string `json:"group,omitempty"`
	HasRelay    bool   `json:"has_relay"`
	HasMetering bool   `json:"has_metering"`
	Caps        int    `json:"outlet_caps,omitempty"`
	Type        int    `json:"outlet_type,omitempty"`
}

type catalogPort struct {
	IfName   string `json:"ifname"`
	Name     string `json:"name"`
	PortIdx  int    `json:"port_idx"`
	Media    string `json:"media"`
	PoECaps  int    `json:"poe_caps,omitempty"`
	IsUplink bool   `json:"is_uplink,omitempty"`
}

type catalogRadio struct {
	Name        string `json:"name"`
	Radio       string `json:"radio"`
	HT          string `json:"ht"`
	MinTxPower  int    `json:"min_txpower"`
	MaxTxPower  int    `json:"max_txpower"`
	NSS         int    `json:"nss"`
	RadioCaps   int    `json:"radio_caps"`
	AntennaGain int    `json:"antenna_gain"`
}

type rawEnvelope struct {
	Meta struct {
		RC  string `json:"rc"`
		Msg string `json:"msg"`
	} `json:"meta"`
	Data []rawDevice `json:"data"`
}

type rawDevice struct {
	Model         string         `json:"model"`
	Type          string         `json:"type"`
	Name          string         `json:"name"`
	Version       string         `json:"version"`
	State         int            `json:"state"`
	Adopted       bool           `json:"adopted"`
	PortTable     []catalogPort  `json:"port_table"`
	RadioTable    []catalogRadio `json:"radio_table"`
	EthernetTable []struct {
		NumPort int `json:"num_port"`
	} `json:"ethernet_table"`
}

type deviceDBModel struct {
	Type         string                     `json:"type"`
	SysID        string                     `json:"systemIdHexadecimal"`
	Adoptability string                     `json:"adoptability"`
	Ports        map[string]json.RawMessage `json:"ports"`
	// Outlets mirrors Ports: a map of category to the indexes in it, in the
	// same three encodings (a count, an index array, or a range string).
	Outlets map[string]json.RawMessage `json:"outlets"`
	Radios  map[string]struct {
		MaxPower int `json:"maxPower"`
		Gain     int `json:"gain"`
	} `json:"radios"`
	Features struct {
		PoE bool `json:"poe"`
	} `json:"features"`
	// DeviceCapabilities names what a model can do in coarse terms. It is
	// the only structural source for the facts an outlet layout cannot
	// carry on its own: whether the outlets meter, and whether the device
	// is battery-backed.
	DeviceCapabilities []string `json:"deviceCapabilities"`
	LinkNegotiation    map[string]struct {
		PortIdx         int      `json:"portIdx"`
		SupportedValues []string `json:"supportedValues"`
	} `json:"linkNegotiation"`
}

func reduceDeviceDatabase(identity, bundle io.Reader, controllerVersion string) (catalogFile, error) {
	if strings.TrimSpace(controllerVersion) == "" {
		return catalogFile{}, errors.New("controller version is required")
	}
	var raw rawEnvelope
	if err := json.NewDecoder(identity).Decode(&raw); err != nil {
		return catalogFile{}, fmt.Errorf("decode stat/device identity dump: %w", err)
	}
	if raw.Meta.RC != "" && raw.Meta.RC != "ok" {
		return catalogFile{}, fmt.Errorf("stat/device: %s", raw.Meta.Msg)
	}
	bundleBytes, err := io.ReadAll(bundle)
	if err != nil {
		return catalogFile{}, fmt.Errorf("read device database bundle: %w", err)
	}

	out := catalogFile{
		ControllerVersion: controllerVersion,
		IdentitySource:    "GET /api/s/default/stat/device",
		HardwareSource:    "controller UI device database bundle",
	}
	seen := make(map[string]struct{}, len(raw.Data))
	for _, d := range raw.Data {
		if d.Model == "" || d.Type == "" || d.Version == "" {
			return catalogFile{}, fmt.Errorf("incomplete device identity: model=%q type=%q version=%q",
				d.Model, d.Type, d.Version)
		}
		if _, ok := seen[d.Model]; ok {
			return catalogFile{}, fmt.Errorf("duplicate model %s", d.Model)
		}
		seen[d.Model] = struct{}{}
		metaJSON, err := extractModelJSON(bundleBytes, d.Model)
		if err != nil {
			return catalogFile{}, err
		}
		var meta deviceDBModel
		if err := json.Unmarshal(metaJSON, &meta); err != nil {
			return catalogFile{}, fmt.Errorf("decode device database model %s: %w", d.Model, err)
		}
		if meta.Type != d.Type {
			return catalogFile{}, fmt.Errorf("model %s type mismatch: stat/device=%q device database=%q",
				d.Model, d.Type, meta.Type)
		}
		m := catalogModel{
			Model:        d.Model,
			ModelDisplay: displayName(d),
			Type:         d.Type,
			Version:      d.Version,
		}
		switch d.Type {
		case "ugw":
			m.Ports, err = gatewayPorts(meta)
		case "usw":
			m.Ports, err = switchMetadataPorts(meta, modelOverride{})
		case "uap":
			m.Ports = accessPointPorts(d.Model)
			m.Radios = metadataRadios(meta)
		default:
			err = fmt.Errorf("model %s has unsupported type %q", d.Model, d.Type)
		}
		if err != nil {
			return catalogFile{}, fmt.Errorf("model %s: %w", d.Model, err)
		}
		if err := validateModel(&m); err != nil {
			return catalogFile{}, err
		}
		out.Models = append(out.Models, m)
	}
	sort.Slice(out.Models, func(i, j int) bool {
		return out.Models[i].Model < out.Models[j].Model
	})
	return out, nil
}

// modelKeyPattern matches a quoted, uppercase model-code key that opens an
// object: `"USW48POE": {`. Whitespace between the key, colon, and brace is
// tolerated because the hardware DB bundle is hand-formatted JavaScript.
var modelKeyPattern = regexp.MustCompile(`"([A-Z0-9][A-Z0-9-]{2,12})"\s*:\s*\{`)

// allModelKeys returns every quoted model-code key in the bundle whose object
// carries a top-level "type" field, sorted and de-duplicated. The regexp only
// finds candidates; extractModelJSON confirms the object shape and the type
// probe rejects container objects (e.g. "models", "features") that merely look
// like model keys.
func allModelKeys(bundle []byte) []string {
	seen := map[string]bool{}
	var keys []string
	for _, match := range modelKeyPattern.FindAllSubmatch(bundle, -1) {
		key := string(match[1])
		if seen[key] {
			continue
		}
		obj, err := extractModelJSON(bundle, key)
		if err != nil {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(obj, &probe) != nil || probe.Type == "" {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// displayFor returns the human-facing model name from the display map, falling
// back to the bare model code when the bundle offers no friendly name. An
// override wins over both: the bundle's table carries a few internal names for
// models that shipped under a different one, and the device reports the name it
// was sold as.
func displayFor(model string, display map[string]string, o modelOverride) string {
	if o.Display != "" {
		return o.Display
	}
	if name, ok := display[model]; ok && name != "" {
		return name
	}
	return model
}

// deriveLayout fills in ports (and radios for APs) from the hardware DB
// metadata using the existing generic derivation helpers. m.Model and m.Type
// must already be set.
func deriveLayout(m *catalogModel, meta deviceDBModel, o modelOverride) error {
	var err error
	switch m.Type {
	case "ugw", "uxg":
		m.Ports, err = gatewayPorts(meta)
	case "usw", "usp":
		m.Ports, err = switchMetadataPorts(meta, o)
	case "uap":
		m.Ports = accessPointPorts(m.Model)
		m.Radios = metadataRadios(meta)
	default:
		err = fmt.Errorf("unsupported type %q", m.Type)
	}
	if err != nil {
		return err
	}
	// Outlets are derived for every type, not inside the switch above: the
	// power lineup is spread across usw, uap, uxg and usp, so a model's
	// outlet map is the only thing that says whether it has outlets.
	m.Outlets, err = deriveOutlets(meta)
	if err != nil {
		return err
	}
	m.SmartPowerCaps = deriveSmartPowerCaps(meta)
	// A model that has outlets must say so here as well as listing them.
	// The controller reads this bitmap as the statement of what hardware
	// exists and ignores an outlet table from a device that does not claim
	// the outlet bit.
	if len(m.Outlets) > 0 {
		m.HWCaps |= hwCapOutlet
	}
	return nil
}

// Outlet and smart-power capability bits, mirroring the inform package's
// constants. They are restated here rather than imported for the same reason
// catalogPort restates inform.Port: the generator writes a data file and does
// not otherwise depend on the protocol package.
const (
	outletCapHasRelay   = 1
	outletCapPowerMeter = 2
	outletCapAC         = 65536
	outletCapUSB        = 131072

	smartPowerCapNUTInformationAccess = 1
	smartPowerCapBuzzer               = 4

	hwCapOutlet = 128
)

func hasCapability(meta deviceDBModel, want string) bool {
	for _, c := range meta.DeviceCapabilities {
		if c == want {
			return true
		}
	}
	return false
}

// deriveSmartPowerCaps reports the device-level power bitmap. The capability
// that makes a controller treat a device as a UPS rather than a plain power
// strip is the one that matters: a battery-backed model that omits it loses
// its battery UI entirely.
func deriveSmartPowerCaps(meta deviceDBModel) int {
	caps := 0
	if hasCapability(meta, "BATTERY_MANAGEMENT") {
		caps |= smartPowerCapNUTInformationAccess
	}
	if hasCapability(meta, "BUZZER") {
		caps |= smartPowerCapBuzzer
	}
	return caps
}

// outletGroups are the categories that describe a real outlet, in the order a
// contested index resolves. The remaining categories a model can carry -- lan,
// rj45, wan -- are jacks sharing the same faceplate diagram, not outlets, and
// are skipped.
//
// The order matters because the categories are not disjoint: some models list
// the same index under more than one category, so the outlet set is their
// union rather than their sum. Summing them reports a device with twice the
// outlets it has.
var outletGroups = []string{"usb", "standard", "surge"}

// deriveOutlets builds the outlet layout from the model's outlet map, which
// uses the same index encodings as the port map.
func deriveOutlets(meta deviceDBModel) ([]catalogOutlet, error) {
	if len(meta.Outlets) == 0 {
		return nil, nil
	}
	group := map[int]string{}
	for _, g := range outletGroups {
		raw, ok := meta.Outlets[g]
		if !ok {
			continue
		}
		indexes, err := expandPortIndexes(raw)
		if err != nil {
			return nil, fmt.Errorf("outlet group %q: %w", g, err)
		}
		for _, idx := range indexes {
			if _, taken := group[idx]; !taken {
				group[idx] = g
			}
		}
	}
	if len(group) == 0 {
		return nil, nil
	}
	indexes := make([]int, 0, len(group))
	for idx := range group {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)

	// Whether the outlets meter is a device fact, not an outlet one, and the
	// USB outlets do not meter even on a model whose AC bank does.
	metered := hasCapability(meta, "OUTLET_MONITOR")

	// Which of the two outlet encodings this model speaks. Every model that
	// declares itself MCU-based describes its outlets with the class bits and
	// a has_relay/has_metering pair; the rack PDUs, which do not declare it,
	// use the smaller capability values beside an outlet_type. That split
	// holds across every power model published, and matches what the devices
	// themselves report, but it is a correlation rather than a documented
	// rule -- a model that breaks it would need an override.
	classBits := hasCapability(meta, "MCU_BASED")

	outlets := make([]catalogOutlet, 0, len(indexes))
	for _, idx := range indexes {
		g := group[idx]
		name := fmt.Sprintf("Outlet %d", idx)
		if g == "usb" {
			name = fmt.Sprintf("USB Outlet %d", idx)
		}
		meters := metered && g != "usb"

		caps := outletCapHasRelay
		outletType := 0
		switch {
		case classBits && g == "usb":
			caps |= outletCapUSB
		case classBits:
			caps |= outletCapAC
		case g == "usb":
			// The older encoding carries the outlet's kind in its own
			// field rather than in the capability value.
			outletType = 1
		}
		if meters {
			caps |= outletCapPowerMeter
		}

		outlets = append(outlets, catalogOutlet{
			Index:       idx,
			Name:        name,
			Group:       g,
			HasRelay:    true,
			HasMetering: meters,
			Caps:        caps,
			Type:        outletType,
		})
	}
	return outlets, nil
}

// harvestBundle builds the full-lineup catalog from every in-scope model in the
// controller hardware DB bundle. Unlike reduceDeviceDatabase, which is gated on
// an adopted stat/device dump, this walks every quoted model key with a
// top-level type of uap/usw/ugw. Ports and radios are derived generically; the
// firmware index supplies versions, the display map supplies friendly names,
// and overrides patch the AP ethernet layout and per-band spatial streams that
// the bundle cannot express.
// catalogSource names where a harvest's model metadata came from, so the
// generated catalog carries its own provenance instead of leaving the next
// reader to guess which input produced it.
type catalogSource struct {
	Identity string
	Hardware string
	// StrictOverrides fails the run when an override names a model the
	// source does not carry. That check exists to catch a typo in the
	// overrides file, and it only means that against the controller's own
	// database, which is the authority on which models exist. The public
	// database covers a different set, so the same condition there is a
	// coverage gap and is reported instead.
	StrictOverrides bool
}

var (
	sourceBundle = catalogSource{
		Identity:        "controller hardware DB bundle (swai.js)",
		Hardware:        "controller hardware DB bundle + fw-update API + tech specs",
		StrictOverrides: true,
	}
	sourceFingerprint = catalogSource{
		Identity: "Ubiquiti device fingerprint DB (static.ui.com/fingerprint/ui/public.json)",
		Hardware: "Ubiquiti device fingerprint DB + fw-update API + tech specs",
	}
	sourceAgreed = catalogSource{
		Identity: "controller hardware DB bundle (swai.js), agreeing with the Ubiquiti device fingerprint DB (static.ui.com/fingerprint/ui/public.json)",
		Hardware: "controller hardware DB bundle + Ubiquiti device fingerprint DB + fw-update API + tech specs",
	}
)

// bundleModels parses the per-model metadata out of the controller UI bundle.
// A model whose entry can't be located or parsed is dropped here rather than
// failing the run, matching how the harvest has always treated the bundle.
func bundleModels(bundle []byte) map[string]deviceDBModel {
	models := map[string]deviceDBModel{}
	for _, model := range allModelKeys(bundle) {
		metaJSON, err := extractModelJSON(bundle, model)
		if err != nil {
			continue
		}
		var meta deviceDBModel
		if err := json.Unmarshal(metaJSON, &meta); err != nil {
			continue
		}
		models[model] = meta
	}
	return models
}

func harvestBundle(bundle []byte, display map[string]string, fw firmwareIndex, ov overrides, caps capsFile, ver string) (catalogFile, error) {
	return harvestModels(bundleModels(bundle), display, fw, ov, caps, ver, sourceBundle)
}

func harvestModels(models map[string]deviceDBModel, display map[string]string, fw firmwareIndex, ov overrides, caps capsFile, ver string, src catalogSource) (catalogFile, error) {
	out := catalogFile{
		ControllerVersion: ver,
		IdentitySource:    src.Identity,
		HardwareSource:    src.Hardware,
	}
	// considered holds every in-scope model the bundle offers, including ones
	// later skipped as inexpressible. Overrides are validated against this set,
	// not just the successfully-generated models: an override for a real model
	// the bundle can't yet render (e.g. a novel radio band) is legitimate, but
	// an override for a typo or an out-of-scope model is stale.
	considered := map[string]bool{}
	var skipped, defaultedEth, excluded []string
	codes := make([]string, 0, len(models))
	for model := range models {
		codes = append(codes, model)
	}
	sort.Strings(codes)
	for _, model := range codes {
		meta := models[model]
		switch meta.Type {
		case "uap", "usw", "ugw", "uxg", "usp":
		default:
			// Out of scope. udm/uck consoles run the Network app themselves
			// (adoptability "standalone"); a controller receives their inform
			// but never lists them as pending, so they can't adopt via this
			// path (a UDM-Pro never reaches state=2). unvr/unas
			// and other classes report differently.
			continue
		}
		considered[model] = true

		// The bundle's own verdict, when it disagrees with the type filter:
		// a console that reports an emulatable type would otherwise reach the
		// catalog and never adopt. No current model takes this path.
		if meta.Adoptability != "" && meta.Adoptability != "adoptable" {
			skipped = append(skipped, fmt.Sprintf("%s (adoptability %q)", model, meta.Adoptability))
			continue
		}
		// Models measured not to adopt despite claiming they can.
		if reason, ok := excludedModels[model]; ok {
			excluded = append(excluded, fmt.Sprintf("%s (%s)", model, reason))
			continue
		}

		// Looked up before derivation because a port override changes what
		// the layout derives, not just what is patched onto it afterwards.
		o, hasOverride := ov.Models[model]
		m := catalogModel{
			Model:        model,
			ModelDisplay: displayFor(model, display, o),
			Type:         meta.Type,
			Version:      firmwareVersion(fw, model, meta.Type),
		}
		// A model the bundle can't express (unknown radio band, unusual port
		// encoding) is omitted, not silently wrong, and not fatal to the whole
		// lineup. It's logged so the gap is visible and closable with an
		// override or a supported-band addition.
		if err := deriveLayout(&m, meta, o); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", model, err))
			continue
		}
		// fw_caps comes from the firmware branch, so it is looked up by
		// type@version and applies to every model sharing that build. A
		// model measured to differ from its branch-mates can say so with
		// a model@version entry, which wins.
		if fc, ok := ov.FirmwareCaps[m.Model+"@"+m.Version]; ok {
			m.FWCaps = fc.FWCaps
		} else if fc, ok := ov.FirmwareCaps[m.Type+"@"+m.Version]; ok {
			m.FWCaps = fc.FWCaps
		}
		if hasOverride {
			applyOverride(&m, o)
			if o.UDAPI != nil {
				mask, err := caps.udapiMask(model, o.UDAPI.Caps)
				if err != nil {
					return catalogFile{}, err
				}
				// Both keys or neither. A device reporting the bitmap
				// with no version has its whole capability update
				// skipped by the controller, so half an override is
				// worse than none.
				m.UDAPIVersion = o.UDAPI.Version
				m.UDAPICaps = mask
			}
		}
		if err := validateModel(&m); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", model, err))
			continue
		}
		// accessPointPorts always yields a valid layout, so an AP without an
		// eth override keeps its derived ports; record that it fell back.
		if meta.Type == "uap" && (!hasOverride || o.Eth == nil) {
			defaultedEth = append(defaultedEth, model)
		}
		sort.Slice(m.Ports, func(i, j int) bool { return m.Ports[i].PortIdx < m.Ports[j].PortIdx })
		out.Models = append(out.Models, m)
	}
	sort.Slice(out.Models, func(i, j int) bool {
		return out.Models[i].Model < out.Models[j].Model
	})
	if len(defaultedEth) > 0 {
		fmt.Fprintf(os.Stderr, "modelgen: %d APs using default ethernet (no eth override): %v\n",
			len(defaultedEth), defaultedEth)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "modelgen: skipped %d models the source can't express:\n", len(skipped))
		for _, s := range skipped {
			fmt.Fprintf(os.Stderr, "  - %s\n", s)
		}
	}
	if len(excluded) > 0 {
		fmt.Fprintf(os.Stderr, "modelgen: excluded %d models that do not adopt:\n", len(excluded))
		for _, s := range excluded {
			fmt.Fprintf(os.Stderr, "  - %s\n", s)
		}
	}
	if stale := staleExclusions(considered); len(stale) > 0 {
		fmt.Fprintf(os.Stderr, "modelgen: %d exclusions name models absent from this source: %v\n",
			len(stale), stale)
	}
	out.ExcludedModels = excludedCatalogList()
	if err := checkStaleOverrides(ov, considered); err != nil {
		if src.StrictOverrides {
			return catalogFile{}, err
		}
		fmt.Fprintf(os.Stderr, "modelgen: %v\n", err)
	}
	return out, nil
}

func extractModelJSON(bundle []byte, model string) ([]byte, error) {
	key := []byte(strconv.Quote(model))
	searchFrom := 0
	start := -1
	for searchFrom < len(bundle) {
		at := bytes.Index(bundle[searchFrom:], key)
		if at < 0 {
			break
		}
		at += searchFrom + len(key)
		for at < len(bundle) && (bundle[at] == ' ' || bundle[at] == '\t' ||
			bundle[at] == '\r' || bundle[at] == '\n') {
			at++
		}
		if at < len(bundle) && bundle[at] == ':' {
			at++
			for at < len(bundle) && (bundle[at] == ' ' || bundle[at] == '\t' ||
				bundle[at] == '\r' || bundle[at] == '\n') {
				at++
			}
			if at < len(bundle) && bundle[at] == '{' {
				start = at
				break
			}
		}
		searchFrom = at
	}
	if start < 0 {
		return nil, fmt.Errorf("model %s missing from controller device database", model)
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(bundle); i++ {
		switch c := bundle[i]; {
		case inString && escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case inString && c == '"':
			inString = false
		case inString:
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return bundle[start : i+1], nil
			}
		}
	}
	return nil, fmt.Errorf("model %s has an unterminated metadata object", model)
}

func gatewayPorts(meta deviceDBModel) ([]catalogPort, error) {
	if len(meta.Ports) == 0 {
		return nil, errors.New("gateway has no ports")
	}
	ports := make([]catalogPort, 0, len(meta.Ports))
	for ifName, rawName := range meta.Ports {
		var name string
		// A gateway's ports map holds ifname->role ("eth0":"WAN"). UXG/UDM
		// entries also carry switch-category keys ("standard":[0,1]) whose
		// values are not strings — skip those; the ifname entries are the ports.
		if err := json.Unmarshal(rawName, &name); err != nil {
			continue
		}
		// Ethernet ports only; some gateways list a power-supply pseudo-port
		// ("psu0") that carries no portIdx and isn't a network interface.
		if !strings.HasPrefix(ifName, "eth") {
			continue
		}
		neg := meta.LinkNegotiation[ifName]
		idx := neg.PortIdx
		if idx == 0 {
			n, err := strconv.Atoi(strings.TrimPrefix(ifName, "eth"))
			if err != nil {
				return nil, fmt.Errorf("gateway port %q has no portIdx", ifName)
			}
			idx = n + 1
		}
		ports = append(ports, catalogPort{
			IfName: ifName, Name: name, PortIdx: idx,
			Media: negotiatedMedia(neg.SupportedValues), IsUplink: idx == 1,
		})
	}
	if len(ports) == 0 {
		return nil, errors.New("gateway has no ethernet ports")
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].PortIdx < ports[j].PortIdx })
	return ports, nil
}

// negotiatedMedia maps a port's link-negotiation speeds to a media label,
// taking the fastest supported RJ45 rate. Defaults to GE when unknown.
func negotiatedMedia(supported []string) string {
	best := 0
	for _, s := range supported {
		for _, tok := range strings.Fields(s) {
			if n, err := strconv.Atoi(tok); err == nil && n > best {
				best = n
			}
		}
	}
	switch {
	case best >= 10000:
		return "10GbE"
	case best >= 2500:
		return "2.5GbE"
	default:
		return "GE"
	}
}

// switchMetadataPorts builds a switch port layout from the hardware DB's
// per-category index map. Categories are walked copper-first, so a fiber
// category wins an index a copper one also claims -- that is how a combo
// pair the bundle records as two copper ports becomes copper plus fiber.
// The override lets a category's media or index set be restated where the
// bundle contradicts the product's published port layout, which it does
// for a handful of models.
func switchMetadataPorts(meta deviceDBModel, o modelOverride) ([]catalogPort, error) {
	mediaByIndex := map[int]string{}
	for _, category := range []struct {
		name  string
		media string
	}{
		{"standard", "GE"},
		{"sfp", "SFP"},
		{"plus", "SFP+"},
		{"sfp28", "SFP28"},
		{"qsfp28", "QSFP28"},
		// Copper above a gigabit. The source describes every RJ45 port as
		// "standard" whatever it negotiates, so a switch with a mixed
		// copper bank -- twelve gigabit ports beside twelve 2.5G ones, or
		// a 2.5G bank with a 10G uplink port -- cannot say so through
		// that one category. These carry no ports on their own and exist
		// for an override to move indexes into, which is the only way the
		// faster half of such a bank gets described.
		{"multigig2.5", "2.5GbE"},
		{"multigig10", "10GbE"},
	} {
		po, patched := o.Ports[category.name]
		raw, ok := meta.Ports[category.name]
		// An override may introduce a category the bundle omits, so an
		// absent bundle entry is only fatal when nothing supplies indexes.
		if !ok && (!patched || po.Indexes == "") {
			continue
		}
		if patched && po.Indexes != "" {
			raw = json.RawMessage(strconv.Quote(po.Indexes))
		}
		indexes, err := expandPortIndexes(raw)
		if err != nil {
			return nil, fmt.Errorf("%s ports: %w", category.name, err)
		}
		media := category.media
		if patched && po.Media != "" {
			media = po.Media
		}
		for _, idx := range indexes {
			mediaByIndex[idx] = media
		}
	}
	if len(mediaByIndex) == 0 {
		return nil, errors.New("switch has no recognized ports")
	}
	poe := meta.Features.PoE
	if o.PoE != nil {
		poe = *o.PoE
	}
	indexes := make([]int, 0, len(mediaByIndex))
	for idx := range mediaByIndex {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	ports := make([]catalogPort, 0, len(indexes))
	for _, idx := range indexes {
		// PoE runs over copper, at whatever speed the port negotiates.
		// Testing for gigabit specifically was safe only while every
		// copper port was described as gigabit; a 2.5G or 10G PoE port
		// would silently lose its power.
		poeCaps := 0
		if poe && isCopper(mediaByIndex[idx]) {
			poeCaps = 7
		}
		ports = append(ports, catalogPort{
			IfName:   fmt.Sprintf("eth%d", idx-1),
			Name:     fmt.Sprintf("Port %d", idx),
			PortIdx:  idx,
			Media:    mediaByIndex[idx],
			PoECaps:  poeCaps,
			IsUplink: idx == 1,
		})
	}
	return ports, nil
}

func expandPortIndexes(raw json.RawMessage) ([]int, error) {
	var count int
	if json.Unmarshal(raw, &count) == nil {
		if count <= 0 {
			return nil, fmt.Errorf("invalid count %d", count)
		}
		out := make([]int, count)
		for i := range out {
			out[i] = i + 1
		}
		return out, nil
	}
	var indexes []int
	if json.Unmarshal(raw, &indexes) == nil {
		if len(indexes) == 0 {
			return nil, errors.New("empty index list")
		}
		for _, idx := range indexes {
			if idx <= 0 {
				return nil, fmt.Errorf("invalid index %d", idx)
			}
		}
		return indexes, nil
	}
	var ranges string
	if err := json.Unmarshal(raw, &ranges); err != nil {
		return nil, fmt.Errorf("unsupported port encoding %s", raw)
	}
	var out []int
	for _, part := range strings.Split(ranges, ",") {
		part = strings.TrimSpace(part)
		loText, hiText, hasRange := strings.Cut(part, "-")
		lo, err := strconv.Atoi(loText)
		if err != nil || lo <= 0 {
			return nil, fmt.Errorf("invalid port range %q", part)
		}
		hi := lo
		if hasRange {
			hi, err = strconv.Atoi(hiText)
			if err != nil || hi < lo {
				return nil, fmt.Errorf("invalid port range %q", part)
			}
		}
		for idx := lo; idx <= hi; idx++ {
			out = append(out, idx)
		}
	}
	return out, nil
}

func accessPointPorts(model string) []catalogPort {
	count := 1
	media := "GE"
	switch model {
	case "U7MP":
		count = 2
	case "U7PRO", "UAPA6B0":
		media = "2.5GbE"
	}
	ports := make([]catalogPort, 0, count)
	for idx := 1; idx <= count; idx++ {
		ports = append(ports, catalogPort{
			IfName: fmt.Sprintf("eth%d", idx-1), Name: fmt.Sprintf("eth%d", idx-1),
			PortIdx: idx, Media: media, IsUplink: idx == 1,
		})
	}
	return ports
}

func metadataRadios(meta deviceDBModel) []catalogRadio {
	order := map[string]int{"ng": 0, "na": 1, "6e": 2}
	// The bundle carries power and gain per band but never the stream
	// count, so every radio starts at the commonest width and the models
	// that ship 1x1, 3x3 or 4x4 silicon are corrected by override. Keeping
	// the exceptions in one file beats scattering them through here.
	const nss = 2
	radios := make([]catalogRadio, 0, len(meta.Radios))
	for band, facts := range meta.Radios {
		ht := "40"
		switch band {
		case "ng":
			ht = "20"
		case "6e":
			ht = "80"
		}
		radios = append(radios, catalogRadio{
			Name: "wifi-" + band, Radio: band, HT: ht,
			MinTxPower: 5, MaxTxPower: facts.MaxPower, NSS: nss,
			AntennaGain: facts.Gain,
		})
	}
	sort.Slice(radios, func(i, j int) bool {
		return order[radios[i].Radio] < order[radios[j].Radio]
	})
	return radios
}

func reduce(r io.Reader, controllerVersion string) (catalogFile, error) {
	if strings.TrimSpace(controllerVersion) == "" {
		return catalogFile{}, errors.New("controller version is required")
	}
	var raw rawEnvelope
	dec := json.NewDecoder(r)
	if err := dec.Decode(&raw); err != nil {
		return catalogFile{}, fmt.Errorf("decode stat/device: %w", err)
	}
	if raw.Meta.RC != "" && raw.Meta.RC != "ok" {
		return catalogFile{}, fmt.Errorf("stat/device: %s", raw.Meta.Msg)
	}

	out := catalogFile{
		ControllerVersion: controllerVersion,
		IdentitySource:    "GET /api/s/default/stat/device",
		HardwareSource:    "adopted stat/device port_table and radio_table",
	}
	seen := make(map[string]struct{}, len(raw.Data))
	for _, d := range raw.Data {
		if d.Model == "" || d.Type == "" || d.Version == "" {
			return catalogFile{}, fmt.Errorf("incomplete device identity: model=%q type=%q version=%q",
				d.Model, d.Type, d.Version)
		}
		if d.State != 1 || !d.Adopted {
			return catalogFile{}, fmt.Errorf("model %s is not adopted and connected", d.Model)
		}
		if _, ok := seen[d.Model]; ok {
			return catalogFile{}, fmt.Errorf("duplicate model %s", d.Model)
		}
		seen[d.Model] = struct{}{}

		m := catalogModel{
			Model:        d.Model,
			ModelDisplay: displayName(d),
			Type:         d.Type,
			Version:      d.Version,
			Ports:        append([]catalogPort(nil), d.PortTable...),
			Radios:       append([]catalogRadio(nil), d.RadioTable...),
		}
		if len(m.Ports) == 0 && len(d.EthernetTable) > 0 {
			for i := 1; i <= d.EthernetTable[0].NumPort; i++ {
				m.Ports = append(m.Ports, catalogPort{
					IfName:   fmt.Sprintf("eth%d", i-1),
					Name:     fmt.Sprintf("Port %d", i),
					PortIdx:  i,
					Media:    "GE",
					IsUplink: i == 1,
				})
			}
		}
		if err := validateModel(&m); err != nil {
			return catalogFile{}, err
		}
		sort.Slice(m.Ports, func(i, j int) bool {
			return m.Ports[i].PortIdx < m.Ports[j].PortIdx
		})
		out.Models = append(out.Models, m)
	}
	sort.Slice(out.Models, func(i, j int) bool {
		return out.Models[i].Model < out.Models[j].Model
	})
	return out, nil
}

func displayName(d rawDevice) string {
	if d.Name != "" {
		if hw, err := net.ParseMAC(d.Name); err != nil || len(hw) != 6 {
			return d.Name
		}
	}
	return d.Model
}

func validateModel(m *catalogModel) error {
	if m.Model == "" || m.ModelDisplay == "" || m.Version == "" {
		return fmt.Errorf("incomplete model identity: model=%q display=%q version=%q",
			m.Model, m.ModelDisplay, m.Version)
	}
	switch m.Type {
	case "ugw", "usw", "uap", "uxg", "usp":
	default:
		return fmt.Errorf("model %s has unsupported type %q", m.Model, m.Type)
	}
	// Ports are how almost every model is described, but not all: some power
	// devices report outlets and no ethernet layout at all. A model with
	// neither has nothing to describe it and stays out of the catalog.
	if len(m.Ports) == 0 && len(m.Outlets) == 0 {
		return fmt.Errorf("model %s has neither ports nor outlets", m.Model)
	}
	portIndexes := make(map[int]struct{}, len(m.Ports))
	ifNames := make(map[string]struct{}, len(m.Ports))
	uplinks := 0
	for i := range m.Ports {
		p := &m.Ports[i]
		if p.PortIdx <= 0 {
			return fmt.Errorf("model %s has invalid port index %d", m.Model, p.PortIdx)
		}
		if _, ok := portIndexes[p.PortIdx]; ok {
			return fmt.Errorf("model %s has duplicate port index %d", m.Model, p.PortIdx)
		}
		portIndexes[p.PortIdx] = struct{}{}
		if p.IfName == "" {
			p.IfName = fmt.Sprintf("eth%d", p.PortIdx-1)
		}
		if _, ok := ifNames[p.IfName]; ok {
			return fmt.Errorf("model %s has duplicate ifname %q", m.Model, p.IfName)
		}
		ifNames[p.IfName] = struct{}{}
		if p.Name == "" {
			p.Name = fmt.Sprintf("Port %d", p.PortIdx)
		}
		if p.Media == "" {
			p.Media = "GE"
		}
		if p.IsUplink {
			uplinks++
		}
	}
	// Exactly one port carries the uplink flag -- but only where there are
	// ports to flag.
	if len(m.Ports) > 0 && uplinks != 1 {
		return fmt.Errorf("model %s has %d uplink ports, want exactly one", m.Model, uplinks)
	}
	outletIndexes := make(map[int]struct{}, len(m.Outlets))
	for i := range m.Outlets {
		o := &m.Outlets[i]
		if o.Index <= 0 {
			return fmt.Errorf("model %s has invalid outlet index %d", m.Model, o.Index)
		}
		if _, ok := outletIndexes[o.Index]; ok {
			return fmt.Errorf("model %s has duplicate outlet index %d", m.Model, o.Index)
		}
		outletIndexes[o.Index] = struct{}{}
		if o.Name == "" {
			o.Name = fmt.Sprintf("Outlet %d", o.Index)
		}
	}
	if m.Type == "uap" && len(m.Radios) == 0 {
		return fmt.Errorf("model %s has no radios", m.Model)
	}
	radios := make(map[string]struct{}, len(m.Radios))
	for _, r := range m.Radios {
		if r.Name == "" || r.Radio == "" || r.HT == "" {
			return fmt.Errorf("model %s has incomplete radio %+v", m.Model, r)
		}
		switch r.Radio {
		case "ng", "na", "6e":
		default:
			return fmt.Errorf("model %s has unsupported radio band %q", m.Model, r.Radio)
		}
		if r.MinTxPower <= 0 || r.MaxTxPower < r.MinTxPower || r.NSS <= 0 {
			return fmt.Errorf("model %s has invalid radio facts %+v", m.Model, r)
		}
		if _, ok := radios[r.Name]; ok {
			return fmt.Errorf("model %s has duplicate radio %q", m.Model, r.Name)
		}
		radios[r.Name] = struct{}{}
	}
	return nil
}

func writeCatalog(w io.Writer, catalog catalogFile) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(catalog)
}

func run(args []string) error {
	fs := flag.NewFlagSet("modelgen", flag.ContinueOnError)
	input := fs.String("input", "", "raw stat/device JSON; required unless -bundle is set")
	bundle := fs.String("device-db-bundle", "", "controller UI JavaScript bundle containing the hardware database")
	harvestBundlePath := fs.String("bundle", "", "controller UI JavaScript bundle (swai.js); when set, harvest the full model lineup instead of reducing -input")
	bundlesJSONPath := fs.String("bundles-json", "", "bundles.json model->display map, used with -bundle")
	firmwareJSONPath := fs.String("firmware-json", "", "saved firmware-latest.json response, used with -bundle")
	overridesPath := fs.String("overrides", "model_overrides.json", "model overrides file, used with -bundle")
	capsPath := fs.String("caps", "capability_bits.json", "capability bit names from scripts/extract_caps.py, used with -bundle")
	catalogPath := fs.String("catalog", "model_profiles.json", "reduced model catalog")
	version := fs.String("controller-version", "", "source controller version (required with -input or -bundle)")
	fetchEth := fs.Bool("fetch-eth", false, "pull AP ethernet from Tech Specs into -overrides (needs -bundle and -fingerprint)")
	fingerprintPath := fs.String("fingerprint", "", "Ubiquiti device fingerprint DB (static.ui.com/fingerprint/ui/public.json): a harvest source on its own, a cross-check when given with -bundle, and the SKU index for -fetch-eth")
	allowDisagreement := fs.Bool("allow-source-disagreement", false, "harvest from -bundle even where -fingerprint describes a model differently, instead of stopping")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *fetchEth {
		if *harvestBundlePath == "" || *fingerprintPath == "" {
			return fmt.Errorf("-fetch-eth requires -bundle and -fingerprint")
		}
		return runFetchEth(*harvestBundlePath, *fingerprintPath, *overridesPath)
	}

	// The two modes read different things and produce different catalogs, so
	// combining them means one flag is silently doing nothing.
	if *input != "" && (*harvestBundlePath != "" || *fingerprintPath != "") {
		return errors.New("-input reduces an adopted fleet dump; -bundle and -fingerprint harvest the full lineup: pass one mode, not both")
	}

	var catalog catalogFile
	if *harvestBundlePath != "" || *fingerprintPath != "" {
		if strings.TrimSpace(*version) == "" {
			return errors.New("controller version is required (-controller-version)")
		}
		var bundleBytes []byte
		if *harvestBundlePath != "" {
			b, err := os.ReadFile(*harvestBundlePath)
			if err != nil {
				return err
			}
			bundleBytes = b
		}
		var fingerprintBytes []byte
		if *fingerprintPath != "" {
			b, err := os.ReadFile(*fingerprintPath)
			if err != nil {
				return err
			}
			fingerprintBytes = b
		}
		display := map[string]string{}
		if *bundlesJSONPath != "" {
			b, err := os.ReadFile(*bundlesJSONPath)
			if err != nil {
				return err
			}
			var raw map[string]struct {
				Display string `json:"display"`
			}
			if err := json.Unmarshal(b, &raw); err != nil {
				return fmt.Errorf("parse %s: %w", *bundlesJSONPath, err)
			}
			for model, v := range raw {
				display[model] = v.Display
			}
		}
		fw := firmwareIndex{}
		if *firmwareJSONPath != "" {
			f, err := os.Open(*firmwareJSONPath)
			if err != nil {
				return err
			}
			fw, err = parseFirmware(f)
			_ = f.Close()
			if err != nil {
				return err
			}
		}
		ov, err := loadOverrides(*overridesPath)
		if err != nil {
			return err
		}
		caps, err := loadCaps(*capsPath)
		if err != nil {
			return err
		}
		catalog, err = harvestSources(bundleBytes, fingerprintBytes, display, fw, ov, caps, *version, *allowDisagreement)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := writeCatalog(&buf, catalog); err != nil {
			return err
		}
		if err := os.WriteFile(*catalogPath, buf.Bytes(), 0o644); err != nil {
			return err
		}
	} else if *input != "" {
		f, err := os.Open(*input)
		if err != nil {
			return err
		}
		if *bundle != "" {
			db, openErr := os.Open(*bundle)
			if openErr != nil {
				_ = f.Close()
				return openErr
			}
			catalog, err = reduceDeviceDatabase(f, db, *version)
			_ = db.Close()
		} else {
			catalog, err = reduce(f, *version)
		}
		_ = f.Close()
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := writeCatalog(&buf, catalog); err != nil {
			return err
		}
		if err := os.WriteFile(*catalogPath, buf.Bytes(), 0o644); err != nil {
			return err
		}
	} else {
		return errors.New("modelgen: one of -bundle or -input is required")
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "modelgen:", err)
		os.Exit(1)
	}
}

// isCopper reports whether a media token describes an RJ45 port. Power over
// Ethernet and the copper-only features key off this rather than off gigabit,
// which stopped being a synonym for copper once the faster RJ45 ports were
// described as what they are.
func isCopper(media string) bool {
	switch media {
	case "GE", "FE", "2.5GbE", "10GbE":
		return true
	default:
		return false
	}
}
