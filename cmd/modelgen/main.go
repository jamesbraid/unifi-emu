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
	ControllerVersion string         `json:"controller_version"`
	IdentitySource    string         `json:"identity_source"`
	HardwareSource    string         `json:"hardware_source"`
	Models            []catalogModel `json:"models"`
}

type catalogModel struct {
	Model        string         `json:"model"`
	ModelDisplay string         `json:"model_display"`
	Type         string         `json:"type"`
	Version      string         `json:"version"`
	Ports        []catalogPort  `json:"ports,omitempty"`
	Radios       []catalogRadio `json:"radios,omitempty"`
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
	Type   string                     `json:"type"`
	Ports  map[string]json.RawMessage `json:"ports"`
	Radios map[string]struct {
		MaxPower int `json:"maxPower"`
		Gain     int `json:"gain"`
	} `json:"radios"`
	Features struct {
		PoE bool `json:"poe"`
	} `json:"features"`
	LinkNegotiation map[string]struct {
		PortIdx int `json:"portIdx"`
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
			m.Ports, err = switchMetadataPorts(d.Model, meta)
		case "uap":
			m.Ports = accessPointPorts(d.Model)
			m.Radios = metadataRadios(d.Model, meta)
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
// back to the bare model code when the bundle offers no friendly name.
func displayFor(model string, display map[string]string) string {
	if name, ok := display[model]; ok && name != "" {
		return name
	}
	return model
}

// deriveLayout fills in ports (and radios for APs) from the hardware DB
// metadata using the existing generic derivation helpers. m.Model and m.Type
// must already be set.
func deriveLayout(m *catalogModel, meta deviceDBModel) error {
	var err error
	switch m.Type {
	case "ugw":
		m.Ports, err = gatewayPorts(meta)
	case "usw":
		m.Ports, err = switchMetadataPorts(m.Model, meta)
	case "uap":
		m.Ports = accessPointPorts(m.Model)
		m.Radios = metadataRadios(m.Model, meta)
	default:
		err = fmt.Errorf("unsupported type %q", m.Type)
	}
	return err
}

// harvestBundle builds the full-lineup catalog from every in-scope model in the
// controller hardware DB bundle. Unlike reduceDeviceDatabase, which is gated on
// an adopted stat/device dump, this walks every quoted model key with a
// top-level type of uap/usw/ugw. Ports and radios are derived generically; the
// firmware index supplies versions, the display map supplies friendly names,
// and overrides patch the AP ethernet layout and per-band spatial streams that
// the bundle cannot express.
func harvestBundle(bundle []byte, display map[string]string, fw firmwareIndex, ov overrides, ver string) (catalogFile, error) {
	out := catalogFile{
		ControllerVersion: ver,
		IdentitySource:    "controller hardware DB bundle (swai.js)",
		HardwareSource:    "controller hardware DB bundle + fw-update API + tech specs",
	}
	seen := map[string]bool{}
	var skipped, defaultedEth []string
	for _, model := range allModelKeys(bundle) {
		metaJSON, err := extractModelJSON(bundle, model)
		if err != nil {
			continue
		}
		var meta deviceDBModel
		if err := json.Unmarshal(metaJSON, &meta); err != nil {
			continue
		}
		switch meta.Type {
		case "uap", "usw", "ugw":
		default:
			continue // out of scope: udm, unvr, etc.
		}

		m := catalogModel{
			Model:        model,
			ModelDisplay: displayFor(model, display),
			Type:         meta.Type,
			Version:      firmwareVersion(fw, model, meta.Type),
		}
		// A model the bundle can't express (unknown radio band, unusual port
		// encoding) is omitted, not silently wrong, and not fatal to the whole
		// lineup. It's logged so the gap is visible and closable with an
		// override or a supported-band addition.
		if err := deriveLayout(&m, meta); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", model, err))
			continue
		}
		o, hasOverride := ov.Models[model]
		if hasOverride {
			applyOverride(&m, o)
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
		seen[model] = true
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
		fmt.Fprintf(os.Stderr, "modelgen: skipped %d models the bundle can't express:\n", len(skipped))
		for _, s := range skipped {
			fmt.Fprintf(os.Stderr, "  - %s\n", s)
		}
	}
	if err := checkStaleOverrides(ov, seen); err != nil {
		return catalogFile{}, err
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
		if err := json.Unmarshal(rawName, &name); err != nil {
			return nil, fmt.Errorf("gateway port %s name: %w", ifName, err)
		}
		idx := meta.LinkNegotiation[ifName].PortIdx
		if idx == 0 {
			n, err := strconv.Atoi(strings.TrimPrefix(ifName, "eth"))
			if err != nil {
				return nil, fmt.Errorf("gateway port %q has no portIdx", ifName)
			}
			idx = n + 1
		}
		ports = append(ports, catalogPort{
			IfName: ifName, Name: name, PortIdx: idx, Media: "GE", IsUplink: idx == 1,
		})
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].PortIdx < ports[j].PortIdx })
	return ports, nil
}

func switchMetadataPorts(model string, meta deviceDBModel) ([]catalogPort, error) {
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
	} {
		raw, ok := meta.Ports[category.name]
		if !ok {
			continue
		}
		indexes, err := expandPortIndexes(raw)
		if err != nil {
			return nil, fmt.Errorf("%s ports: %w", category.name, err)
		}
		for _, idx := range indexes {
			media := category.media
			// The controller database calls the Ultra's PoE++ input
			// "plus", but it is still a 1 GbE RJ45 port. Other "plus"
			// entries in this catalog are SFP+.
			if model == "USM8P" && category.name == "plus" {
				media = "GE"
			}
			mediaByIndex[idx] = media
		}
	}
	if len(mediaByIndex) == 0 {
		return nil, errors.New("switch has no recognized ports")
	}
	indexes := make([]int, 0, len(mediaByIndex))
	for idx := range mediaByIndex {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	ports := make([]catalogPort, 0, len(indexes))
	for _, idx := range indexes {
		poeCaps := 0
		if meta.Features.PoE && mediaByIndex[idx] == "GE" {
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

func metadataRadios(model string, meta deviceDBModel) []catalogRadio {
	order := map[string]int{"ng": 0, "na": 1, "6e": 2}
	nss := 2
	if model == "U7MP" {
		nss = 3
	}
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
	case "ugw", "usw", "uap":
	default:
		return fmt.Errorf("model %s has unsupported type %q", m.Model, m.Type)
	}
	if len(m.Ports) == 0 {
		return fmt.Errorf("model %s has no ports", m.Model)
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
	if uplinks != 1 {
		return fmt.Errorf("model %s has %d uplink ports, want exactly one", m.Model, uplinks)
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
	catalogPath := fs.String("catalog", "model_profiles.json", "reduced model catalog")
	version := fs.String("controller-version", "", "source controller version (required with -input or -bundle)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var catalog catalogFile
	if *harvestBundlePath != "" {
		bundleBytes, err := os.ReadFile(*harvestBundlePath)
		if err != nil {
			return err
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
		catalog, err = harvestBundle(bundleBytes, display, fw, ov, *version)
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
