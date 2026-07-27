//go:build integration

package emu_test

// What does reporting a real fw_caps actually change?
//
// Every emulated device reports fw_caps 3. That is a placeholder, not a
// measurement: the controller tests 22 distinct bits against fw_caps and
// bits 0 and 1 are not among them, so the emulator's value is
// indistinguishable from 0 or from omitting the key. Adopting a captured
// value would be more faithful, but this project's rule is that every
// claimed capability is a feature the controller will then offer against a
// device that has to service it -- so more faithful is not automatically
// safer, and the difference has never been observed.
//
// Two arms, same AP model on the same controller build, differing only in
// the reported bitmap. Arm "placeholder" sends what ships today. Arm "real"
// sends what a U7-Pro-class AP reports on this exact firmware string. The
// probe records what the controller does differently, and asserts nothing
// about which is better: the point is to see the delta before choosing.
//
// Reading the two device documents side by side is the deliverable, so both
// are written to tmp/itest/<arm>/ along with the controller's own logs.
//
//	UNIFI_EMU_ITEST_FWCAPS=1 go test -tags integration \
//	  -run TestClassicAPFirmwareCapsProbeLive -v -count=1 .

import (
	"context"
	"encoding/json"

	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/internal/adopt"
)

const fwCapsProbeEnv = "UNIFI_EMU_ITEST_FWCAPS"

// fwCapsProbeModel is a U7-Pro on firmware 8.6.11.18870, chosen because the
// captured value below comes from a U7-Pro-class AP reporting that same
// firmware string. Pairing a captured bitmap with a different firmware
// branch would measure two changes at once.
const fwCapsProbeModel = "U7PRO"

const (
	// fwCapsPlaceholder is what every emulated device reports today.
	fwCapsPlaceholder = 3
	// fwCapsRealAP is 0xE7FCFEBF, reported by a real UniFi AP on
	// 8.6.11.18870. Signed because that is how the controller stores an
	// int32; on the wire real firmware sends the unsigned twin, and the
	// controller's getInt truncates either form to the same bits.
	fwCapsRealAP = -402850113
	// fwCapsRealUGW is 0x0042D003, reported by a real USG-3P on
	// 4.4.57.5578372 -- the emulator's own firmware string for the ugw
	// profiles. It sets neither UTERM (4) nor QCASWITCH (16), the two
	// bits that make the controller offer something back at the device.
	fwCapsRealUGW = 4378627
)

// fwCapsProbeGateway is a USG-3P on 4.4.57.5578372, the firmware the
// captured gateway value came from.
const fwCapsProbeGateway = "UGW3"

// fwCapsWrites is the battery each arm attempts. The point is a baseline:
// "no regression" means nothing, unless something succeeded to begin with.
// A write that already fails cannot regress, and one that succeeds in the
// placeholder arm and fails in the real arm is the result that would settle
// the question against adopting a captured value.
var fwCapsWrites = []struct {
	name string
	body map[string]any
}{
	{"rename", map[string]any{"name": "fwcaps-probe"}},
	{"led-override", map[string]any{"led_override": "off"}},
	{"port-overrides-empty", map[string]any{"port_overrides": []map[string]any{}}},
	{"port-overrides-idx", map[string]any{"port_overrides": []map[string]any{{"port_idx": 1}}}},
	{"port-overrides-named", map[string]any{
		"port_overrides": []map[string]any{{"port_idx": 1, "name": "fwcaps-probe"}}}},
}

// fwCapsObservation is what one arm saw. Every field is descriptive: the
// test compares arms rather than asserting any single value, because which
// behaviour is correct is exactly the open question.
type fwCapsObservation struct {
	Arm              string              `json:"arm"`
	Model            string              `json:"model"`
	SentFWCaps       int                 `json:"sent_fw_caps"`
	StoredFWCaps     any                 `json:"stored_fw_caps"`
	Adopted          bool                `json:"adopted"`
	PortTablePresent bool                `json:"port_table_present"`
	PortTableLen     int                 `json:"port_table_len"`
	PortTableKeys    []string            `json:"port_table_keys,omitempty"`
	PortStatsPresent bool                `json:"port_stats_present"`
	Writes           []fwCapsWriteResult `json:"writes"`
}

type fwCapsWriteResult struct {
	Name   string `json:"name"`
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
}

func (o fwCapsObservation) write(name string) fwCapsWriteResult {
	for _, w := range o.Writes {
		if w.Name == name {
			return w
		}
	}
	return fwCapsWriteResult{Name: name}
}

func TestClassicAPFirmwareCapsProbeLive(t *testing.T) {
	if os.Getenv(fwCapsProbeEnv) == "" {
		t.Skipf("fw_caps probe is opt-in: set %s=1 (boots two controllers)", fwCapsProbeEnv)
	}

	// Two device families, because the risky bits are AP-only. The AP arm
	// tests the value that unlocks UTERM and QCASWITCH; the gateway arm
	// tests the value actually proposed for shipping, which sets neither.
	arms := []struct {
		name   string
		model  string
		fwCaps int
	}{
		{"ap-placeholder", fwCapsProbeModel, fwCapsPlaceholder},
		{"ap-real", fwCapsProbeModel, fwCapsRealAP},
		{"gw-placeholder", fwCapsProbeGateway, fwCapsPlaceholder},
		{"gw-real", fwCapsProbeGateway, fwCapsRealUGW},
	}

	got := map[string]fwCapsObservation{}
	all := make([]fwCapsObservation, 0, len(arms))
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			got[arm.name] = runFWCapsArm(t, arm.name, arm.model, arm.fwCaps)
		})
	}
	for _, arm := range arms {
		if o, ok := got[arm.name]; ok {
			all = append(all, o)
		}
	}
	if len(all) != len(arms) {
		t.Fatalf("only %d of %d arms produced an observation", len(all), len(arms))
	}
	report := filepath.Join(evidenceDir(t.Name()), "fwcaps-probe.json")
	if err := writeJSON(report, all); err != nil {
		t.Errorf("write probe report: %v", err)
	}
	t.Logf("full report: %s", report)

	fwCapsCompare(t, "AP", got["ap-placeholder"], got["ap-real"])
	fwCapsCompare(t, "gateway", got["gw-placeholder"], got["gw-real"])
}

// fwCapsCompare reports the delta for one device family and fails only on
// the outcome that would settle the question against adopting a captured
// value: a write that succeeds today and stops succeeding.
func fwCapsCompare(t *testing.T, family string, a, b fwCapsObservation) {
	t.Helper()
	t.Logf("--- %s: fw_caps %v -> %v", family, a.StoredFWCaps, b.StoredFWCaps)
	t.Logf("    port_table len   %d -> %d", a.PortTableLen, b.PortTableLen)
	t.Logf("    port_table keys  %v", a.PortTableKeys)
	t.Logf("                  -> %v", b.PortTableKeys)
	t.Logf("    port_stats kept  %v -> %v", a.PortStatsPresent, b.PortStatsPresent)

	baseline := 0
	for _, want := range fwCapsWrites {
		x, y := a.write(want.name), b.write(want.name)
		t.Logf("    write %-22s %d -> %d   %s", want.name, x.Status, y.Status, y.Body)
		if x.Status == http.StatusOK {
			baseline++
			if y.Status != http.StatusOK {
				t.Errorf("%s: write %q regressed %d -> %d (%s). The validation the real "+
					"fw_caps unlocks rejects a write that works today",
					family, want.name, x.Status, y.Status, y.Body)
			}
		}
	}
	// A battery where nothing succeeded proves nothing about regressions.
	// Say so rather than letting silence read as a clean bill of health.
	if baseline == 0 {
		t.Errorf("%s: no write in the battery succeeded even with the placeholder fw_caps, "+
			"so this arm cannot answer whether the real value breaks a working write", family)
	} else {
		t.Logf("    %d/%d writes succeed on the placeholder and still succeed on the real value",
			baseline, len(fwCapsWrites))
	}
}

func runFWCapsArm(t *testing.T, arm, model string, fwCaps int) fwCapsObservation {
	t.Helper()
	const site = "default"
	obs := fwCapsObservation{Arm: arm, Model: model, SentFWCaps: fwCaps}

	spec := emu.DeviceSpec{
		MAC:    "00:27:22:e0:00:21",
		Model:  model,
		IP:     "192.168.1.221",
		FWCaps: &fwCaps,
	}
	h := startClassicHarness(t)
	h.startEmulator([]emu.DeviceSpec{spec})

	client := adopt.New(h.apiURL, adopt.Classic)
	if err := client.Login(h.ctx, itestControllerUser, itestControllerPassword); err != nil {
		t.Fatalf("login: %v", err)
	}
	device := adoptAndWaitConnected(t, h.ctx, h, client, spec.Model, spec.MAC)
	obs.Adopted = device.State == 1 && device.Adopted

	doc := fwCapsDeviceDoc(t, h.ctx, client, site, spec.MAC)
	if doc == nil {
		t.Fatalf("%s: no device document for %s after adoption", arm, spec.MAC)
	}
	if err := writeJSON(filepath.Join(evidenceDir(t.Name()), "device.json"), doc); err != nil {
		t.Errorf("write device doc: %v", err)
	}

	// Prove the treatment applied before believing anything downstream of
	// it. The first run of this probe reported no difference between the
	// arms because the harness's single-device path had no flag for
	// FWCaps and silently sent the default in both: a green test that
	// measured nothing. An experiment that cannot detect its own
	// independent variable failing to vary is not an experiment.
	obs.StoredFWCaps = doc["fw_caps"]
	stored, ok := obs.StoredFWCaps.(float64)
	if !ok || int(stored) != fwCaps {
		t.Fatalf("%s: controller stored fw_caps=%v but the device was told to report %d; "+
			"the arm did not vary and its measurements are meaningless",
			arm, obs.StoredFWCaps, fwCaps)
	}
	if table, ok := doc["port_table"].([]any); ok {
		obs.PortTablePresent = true
		obs.PortTableLen = len(table)
		if len(table) > 0 {
			if entry, ok := table[0].(map[string]any); ok {
				for k := range entry {
					obs.PortTableKeys = append(obs.PortTableKeys, k)
				}
				sort.Strings(obs.PortTableKeys)
			}
		}
	}
	_, obs.PortStatsPresent = doc["port_stats"]

	obs.Writes = fwCapsRunWrites(t, h.ctx, client, site, doc)
	t.Logf("%s (%s): stored fw_caps=%v port_table=%d keys port_stats=%v",
		arm, model, obs.StoredFWCaps, obs.PortTableLen, obs.PortStatsPresent)
	return obs
}

// fwCapsRunWrites attempts every candidate write against the adopted
// device. Each is restored to the document's own value where it had one, so
// one attempt cannot poison the next.
func fwCapsRunWrites(
	t *testing.T, ctx context.Context, client *adopt.Client, site string, doc map[string]any,
) []fwCapsWriteResult {
	t.Helper()
	id, _ := doc["_id"].(string)
	if id == "" {
		t.Errorf("device document has no _id; no write can be attempted")
		return nil
	}
	results := make([]fwCapsWriteResult, 0, len(fwCapsWrites))
	for _, w := range fwCapsWrites {
		status, body, err := client.DoRaw(ctx, http.MethodPut,
			"/api/s/"+site+"/rest/device/"+id, w.body)
		result := fwCapsWriteResult{Name: w.name, Status: status}
		switch {
		case err != nil:
			result.Body = err.Error()
		case status != http.StatusOK:
			result.Body = fwCapsErrKey(body)
		}
		results = append(results, result)
	}
	return results
}

// fwCapsErrKey pulls the controller's api.err.* key out of a rejection,
// which is the part that distinguishes a capability refusal from a
// malformed request.
func fwCapsErrKey(body []byte) string {
	var envelope struct {
		Meta map[string]any `json:"meta"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if msg, ok := envelope.Meta["msg"].(string); ok {
			return msg
		}
	}
	detail := strings.TrimSpace(string(body))
	if len(detail) > 160 {
		detail = detail[:160]
	}
	return detail
}

// fwCapsDeviceDoc reads the raw stat/device entry for mac. Raw, because the
// typed Device struct keeps only the adoption fields and the whole question
// is what happens to the rest of the document.
func fwCapsDeviceDoc(t *testing.T, ctx context.Context, client *adopt.Client, site, mac string) map[string]any {
	t.Helper()
	status, body, err := client.DoRaw(ctx, http.MethodGet, "/api/s/"+site+"/stat/device", nil)
	if err != nil {
		t.Fatalf("stat/device: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("stat/device: HTTP %d", status)
	}
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("stat/device is not JSON: %v", err)
	}
	for _, doc := range envelope.Data {
		if got, _ := doc["mac"].(string); strings.EqualFold(got, mac) {
			return doc
		}
	}
	return nil
}
