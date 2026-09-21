package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Ubiquiti publishes its device database, unauthenticated, at
// static.ui.com/fingerprint/ui/public.json. Each device carries a
// `unifi.network` object keyed by the same model codes the controller's own
// hardware database uses, describing the same facts in the same encodings --
// so it deserializes straight into deviceDBModel and feeds the same harvest.
//
// It is the better input of the two for anyone who cannot unpack a controller:
// it is first-party, fetchable over HTTP, and citable. It is not a strict
// improvement, though, which is why both sources are kept:
//
//   - it classifies port media coarsely, so a 25G cage and a 10G cage can
//     describe alike;
//   - it lags the shipping product now and then, describing a model at a lower
//     link speed than it sells with;
//   - it carries no adoptability verdict, so the harvest's own type filter and
//     the measured exclusion table are the only guards left;
//   - a handful of models the controller knows are missing from it entirely.
//
// Where the two disagree, neither is automatically right, so a disagreement is
// reported rather than silently resolved.

// fingerprintCatalog is the shape this file reads out of the public database.
type fingerprintCatalog struct {
	Devices []struct {
		Product struct {
			Name string `json:"name"`
		} `json:"product"`
		UniFi struct {
			Network json.RawMessage `json:"network"`
		} `json:"unifi"`
	} `json:"devices"`
}

// fingerprintModels parses the public device database into the per-model
// metadata the harvest consumes, plus the display names it publishes. A device
// with no network model code is not a network device and is skipped.
func fingerprintModels(raw []byte) (map[string]deviceDBModel, map[string]string, error) {
	var doc fingerprintCatalog
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse fingerprint DB: %w", err)
	}
	models := map[string]deviceDBModel{}
	display := map[string]string{}
	for _, dev := range doc.Devices {
		if len(dev.UniFi.Network) == 0 {
			continue
		}
		// The model code lives inside the same object as everything else,
		// so it is read from there rather than from the device's names.
		var ident struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(dev.UniFi.Network, &ident); err != nil || ident.Model == "" {
			continue
		}
		var meta deviceDBModel
		if err := json.Unmarshal(dev.UniFi.Network, &meta); err != nil {
			continue
		}
		models[ident.Model] = meta
		if name := strings.TrimSpace(dev.Product.Name); name != "" {
			display[ident.Model] = name
		}
	}
	if len(models) == 0 {
		return nil, nil, fmt.Errorf("fingerprint DB carried no network models")
	}
	return models, display, nil
}

// compareCatalogs reports where two harvests of the same lineup disagree about
// a model's shape. Models only one source knows are reported separately: the
// two databases genuinely cover different sets, so an absence is a coverage
// fact rather than a conflict.
func compareCatalogs(a, b catalogFile, aName, bName string) (conflicts, onlyA, onlyB []string) {
	index := func(c catalogFile) map[string]catalogModel {
		m := make(map[string]catalogModel, len(c.Models))
		for _, e := range c.Models {
			m[e.Model] = e
		}
		return m
	}
	ma, mb := index(a), index(b)

	codes := make([]string, 0, len(ma))
	for code := range ma {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	for _, code := range codes {
		x, y := ma[code], mb[code]
		if _, ok := mb[code]; !ok {
			onlyA = append(onlyA, code)
			continue
		}
		note := func(field, av, bv string) {
			conflicts = append(conflicts,
				fmt.Sprintf("%s %s: %s says %s, %s says %s", code, field, aName, av, bName, bv))
		}
		if x.Type != y.Type {
			note("type", x.Type, y.Type)
		}
		if len(x.Ports) != len(y.Ports) {
			note("port count", fmt.Sprint(len(x.Ports)), fmt.Sprint(len(y.Ports)))
		} else {
			for i := range x.Ports {
				if x.Ports[i].Media != y.Ports[i].Media {
					note(fmt.Sprintf("port %d media", x.Ports[i].PortIdx),
						x.Ports[i].Media, y.Ports[i].Media)
				}
			}
		}
		if len(x.Outlets) != len(y.Outlets) {
			note("outlet count", fmt.Sprint(len(x.Outlets)), fmt.Sprint(len(y.Outlets)))
		}
		if len(x.Radios) != len(y.Radios) {
			note("radio count", fmt.Sprint(len(x.Radios)), fmt.Sprint(len(y.Radios)))
		}
	}
	for code := range mb {
		if _, ok := ma[code]; !ok {
			onlyB = append(onlyB, code)
		}
	}
	sort.Strings(onlyB)
	return conflicts, onlyA, onlyB
}

// harvestSources builds the catalog from whichever databases were supplied.
//
// With both, each is harvested independently and the results compared. The
// bundle is the one written out -- it is the database the controller itself
// consults, so it is what a device is actually measured against -- and the
// public database serves as the check on it, the way the capability extractor
// reads two controller packagings and requires them to agree. A disagreement
// stops the run, because the alternative is a catalog that silently favours
// one database over another with nothing recording the choice.
func harvestSources(bundle, fingerprint []byte, display map[string]string, fw firmwareIndex,
	ov overrides, caps capsFile, ver string, allowDisagreement bool) (catalogFile, error) {

	switch {
	case len(bundle) == 0 && len(fingerprint) == 0:
		return catalogFile{}, fmt.Errorf("no model database given: pass -bundle, -fingerprint, or both")

	case len(bundle) == 0:
		models, names, err := fingerprintModels(fingerprint)
		if err != nil {
			return catalogFile{}, err
		}
		return harvestModels(models, mergeDisplay(display, names), fw, ov, caps, ver, sourceFingerprint)

	case len(fingerprint) == 0:
		return harvestModels(bundleModels(bundle), display, fw, ov, caps, ver, sourceBundle)
	}

	fpModels, names, err := fingerprintModels(fingerprint)
	if err != nil {
		return catalogFile{}, err
	}
	// The public database publishes a product name for most models, including
	// many the bundle knows only by code, so it is folded in wherever the
	// bundle's own map has nothing to say.
	display = mergeDisplay(display, names)

	fromBundle, err := harvestModels(bundleModels(bundle), display, fw, ov, caps, ver, sourceAgreed)
	if err != nil {
		return catalogFile{}, err
	}
	fromFingerprint, err := harvestModels(fpModels, display, fw, ov, caps, ver, sourceFingerprint)
	if err != nil {
		return catalogFile{}, err
	}

	conflicts, onlyBundle, onlyFingerprint := compareCatalogs(
		fromBundle, fromFingerprint, "bundle", "fingerprint")

	// Coverage differences are expected and are reported without stopping:
	// the two databases are maintained separately and neither is a superset.
	if len(onlyBundle) > 0 {
		fmt.Fprintf(os.Stderr, "modelgen: %d models only the bundle carries: %v\n",
			len(onlyBundle), onlyBundle)
	}
	if len(onlyFingerprint) > 0 {
		fmt.Fprintf(os.Stderr, "modelgen: %d models only the fingerprint DB carries: %v\n",
			len(onlyFingerprint), onlyFingerprint)
	}
	if len(conflicts) > 0 {
		fmt.Fprintf(os.Stderr, "modelgen: %d models described differently by the two databases:\n",
			len(conflicts))
		for _, c := range conflicts {
			fmt.Fprintf(os.Stderr, "  - %s\n", c)
		}
		if !allowDisagreement {
			return catalogFile{}, fmt.Errorf(
				"%d source disagreements; resolve them, or pass -allow-source-disagreement to harvest from the bundle anyway",
				len(conflicts))
		}
		// Harvested from the bundle alone, so the catalog must not claim the
		// two agreed.
		fromBundle.IdentitySource = sourceBundle.Identity
		fromBundle.HardwareSource = sourceBundle.Hardware
	}
	return fromBundle, nil
}

// mergeDisplay returns base with fallback's names filled in behind it, leaving
// any name the primary source already supplies untouched.
func mergeDisplay(base, fallback map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(fallback))
	for k, v := range fallback {
		out[k] = v
	}
	for k, v := range base {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	return out
}
