package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Tech Specs is a Next.js app: product data lives at
// /_next/data/<buildId>/unifi/wifi/<slug>.json, and the buildId rotates on
// every Ubiquiti redeploy. The AP Ethernet layout the hardware DB bundle
// omits is the `networking-interface` spec on each product page.
const (
	techSpecsHome    = "https://techspecs.ui.com/"
	techSpecsDataFmt = "https://techspecs.ui.com/_next/data/%s/unifi/wifi/%s.json"
)

var buildIDPattern = regexp.MustCompile(`"buildId":"([^"]+)"`)

// fetchBuildID reads the current Next.js buildId from the Tech Specs home page.
func fetchBuildID(ctx context.Context) (string, error) {
	body, err := httpGet(ctx, techSpecsHome)
	if err != nil {
		return "", fmt.Errorf("fetch techspecs home: %w", err)
	}
	return parseBuildID(body)
}

// parseBuildID extracts the buildId from a Tech Specs HTML page.
func parseBuildID(html []byte) (string, error) {
	m := buildIDPattern.FindSubmatch(html)
	if m == nil {
		return "", fmt.Errorf("no buildId in techspecs page")
	}
	return string(m[1]), nil
}

// normalizeCode folds a model code or product name to its bare alphanumerics,
// so "U7-Pro", "U7 Pro", and "U7PRO" all compare equal. This is the join key
// between the bundle's model codes and the fingerprint DB's names/SKUs.
func normalizeCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fingerprintIndex maps a normalized model identifier to its Tech Specs SKU.
// Built from Ubiquiti's device fingerprint DB (static.ui.com/fingerprint),
// which is keyed by hardware identity rather than fuzzy display names.
type fingerprintIndex map[string]string

// buildFingerprintIndex indexes every access-point device under the normalized
// form of each of its shortnames, SKU, product abbreviation, and hardware
// sysids, mapping to the SKU (the cleanest Tech Specs slug source). Indexing
// sysids lets hex-coded bundle models (e.g. UAPA6A9, whose code matches no
// name) join by hardware identity — the exact match a device ID provides.
func buildFingerprintIndex(fingerprint []byte) (fingerprintIndex, error) {
	var doc struct {
		Devices []struct {
			DeviceTypes []string `json:"deviceTypes"`
			ShortNames  []string `json:"shortnames"`
			SKU         string   `json:"sku"`
			SysID       string   `json:"sysid"`
			SysIDs      []string `json:"sysids"`
			Product     struct {
				Abbrev string `json:"abbrev"`
			} `json:"product"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(fingerprint, &doc); err != nil {
		return nil, fmt.Errorf("parse fingerprint DB: %w", err)
	}
	idx := fingerprintIndex{}
	for _, d := range doc.Devices {
		isAP := false
		for _, t := range d.DeviceTypes {
			if t == "access-point" {
				isAP = true
				break
			}
		}
		if !isAP || d.SKU == "" {
			continue
		}
		keys := append([]string{d.SKU, d.Product.Abbrev, d.SysID}, d.ShortNames...)
		keys = append(keys, d.SysIDs...)
		for _, n := range keys {
			if key := normalizeCode(n); key != "" {
				// First writer wins so a device's own SKU/shortname isn't
				// clobbered by another device that happens to share an alias.
				if _, ok := idx[key]; !ok {
					idx[key] = d.SKU
				}
			}
		}
	}
	return idx, nil
}

// skuToSlug turns a Tech Specs SKU ("U7-Pro-Max", "U7 Pro") into its URL slug.
func skuToSlug(sku string) string {
	s := strings.ToLower(strings.TrimSpace(sku))
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.Join(strings.Fields(s), "-")
	return s
}

// apCandidate is one AP model and its hardware sysid (from the bundle), the two
// keys used to join it to the fingerprint DB.
type apCandidate struct {
	code  string
	sysID string
}

// ethResult is one AP's resolved Ethernet layout plus the SKU it came from.
type ethResult struct {
	model string
	eth   ethSpec
	sku   string
}

// fetchAPEthernet resolves each AP model's Ethernet layout by matching it to
// the fingerprint DB — first by normalized code, then by hardware sysid —
// deriving the Tech Specs slug from the SKU, and parsing the product's
// networking-interface. It is best-effort: a model with no fingerprint match, a
// 404, or an unparseable value is left out (its GE default stands) and counted
// in unmatched/unresolved.
func fetchAPEthernet(ctx context.Context, aps []apCandidate, idx fingerprintIndex, buildID string) ([]ethResult, int, int) {
	var results []ethResult
	unmatched, unresolved := 0, 0
	for _, ap := range aps {
		sku, ok := idx[normalizeCode(ap.code)]
		if !ok && ap.sysID != "" {
			sku, ok = idx[normalizeCode(ap.sysID)]
		}
		if !ok {
			unmatched++
			continue
		}
		model := ap.code
		url := fmt.Sprintf(techSpecsDataFmt, buildID, skuToSlug(sku))
		body, err := httpGet(ctx, url)
		if err != nil {
			unresolved++
			continue
		}
		eth, ok := parseNetworkingInterface(body)
		if !ok {
			unresolved++
			continue
		}
		results = append(results, ethResult{model: model, eth: eth, sku: sku})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].model < results[j].model })
	return results, unmatched, unresolved
}

// httpGet fetches url and returns its body, erroring on any non-200 status.
func httpGet(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// overridesDoc is model_overrides.json with its provenance note preserved
// across a read/write cycle (the loadOverrides struct drops it).
type overridesDoc struct {
	SourceNote string                   `json:"_source_note,omitempty"`
	Models     map[string]modelOverride `json:"models"`
}

// mergeEthOverrides sets each resolved AP's eth override in the overrides file
// at path, preserving every existing entry and field, and writes it back. It
// returns the number of eth entries added or changed.
func mergeEthOverrides(path string, results []ethResult) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var doc overridesDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Models == nil {
		doc.Models = map[string]modelOverride{}
	}
	changed := 0
	for _, r := range results {
		mo := doc.Models[r.model]
		next := &ethOverride{Count: r.eth.Count, Media: r.eth.Media}
		if mo.Eth == nil || *mo.Eth != *next {
			changed++
		}
		mo.Eth = next
		if mo.Source == "" {
			mo.Source = fmt.Sprintf("techspecs.ui.com networking-interface (fingerprint sku %s)", r.sku)
		}
		doc.Models[r.model] = mo
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return 0, err
	}
	return changed, os.WriteFile(path, append(out, '\n'), 0o644)
}

// runFetchEth pulls AP Ethernet from Tech Specs and writes it into the
// overrides file. bundle supplies the AP model list; fingerprint is Ubiquiti's
// device fingerprint DB.
func runFetchEth(bundlePath, fingerprintPath, overridesPath string) error {
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		return err
	}
	fingerprint, err := os.ReadFile(fingerprintPath)
	if err != nil {
		return err
	}
	idx, err := buildFingerprintIndex(fingerprint)
	if err != nil {
		return err
	}
	var aps []apCandidate
	for _, model := range allModelKeys(bundle) {
		metaJSON, err := extractModelJSON(bundle, model)
		if err != nil {
			continue
		}
		var meta deviceDBModel
		if err := json.Unmarshal(metaJSON, &meta); err != nil {
			continue
		}
		if meta.Type == "uap" {
			aps = append(aps, apCandidate{code: model, sysID: meta.SysID})
		}
	}
	ctx := context.Background()
	buildID, err := fetchBuildID(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "fetch-eth: techspecs buildId %s; %d AP models\n", buildID, len(aps))
	results, unmatched, unresolved := fetchAPEthernet(ctx, aps, idx, buildID)
	changed, err := mergeEthOverrides(overridesPath, results)
	if err != nil {
		return err
	}
	var multigig []string
	for _, r := range results {
		if r.eth.Media != "GE" {
			multigig = append(multigig, fmt.Sprintf("%s=%dx%s", r.model, r.eth.Count, r.eth.Media))
		}
	}
	fmt.Fprintf(os.Stderr, "fetch-eth: resolved %d/%d APs (%d unmatched, %d unresolved), %d overrides changed\n",
		len(results), len(aps), unmatched, unresolved, changed)
	fmt.Fprintf(os.Stderr, "fetch-eth: multi-gig APs: %s\n", strings.Join(multigig, " "))
	return nil
}
