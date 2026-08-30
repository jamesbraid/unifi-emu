package emu

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/inform"
)

const testInformURL = "http://unifi:8080/inform"

// placeholderFWCaps mirrors inform.PlaceholderFWCaps, the fw_caps value a
// device reports when no real bitmap was captured for its firmware. Aliased
// locally so the fw_caps tests read against the canonical constant.
const placeholderFWCaps = inform.PlaceholderFWCaps

func mustDevice(t *testing.T, spec DeviceSpec) *device {
	t.Helper()
	d, err := newDevice(spec, testInformURL)
	if err != nil {
		t.Fatalf("newDevice(%+v): %v", spec, err)
	}
	return d
}

// markAdopted flips a device into the adopted state for payload-shape tests
// (the real transition path via applyResponse is covered by
// device_response_test.go).
func markAdopted(d *device) {
	d.applyResponse([]byte(`{"_type":"cmd","cmd":"set-adopt"}`))
}

func decodePayload(t *testing.T, d *device) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(d.session.BuildPayload(time.Now()), &m); err != nil {
		t.Fatalf("BuildPayload is not valid JSON: %v", err)
	}
	return m
}

func table(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key].([]any)
	if !ok || len(v) == 0 {
		t.Fatalf("%s missing or empty in payload", key)
	}
	return v
}

func TestPayloadSerial(t *testing.T) {
	// A supplied serial is reported verbatim; without one the MAC-derived
	// value stands, so every existing fleet file keeps its serials.
	for _, tc := range []struct {
		name   string
		serial string
		want   string
	}{
		{"supplied", "F09FC2ABCDEF", "F09FC2ABCDEF"},
		{"derived from mac", "", "DC9FDB000001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := mustDevice(t, DeviceSpec{
				MAC: "dc:9f:db:00:00:01", Model: "UGW3", IP: "10.0.0.1", Serial: tc.serial,
			})
			if got := decodePayload(t, d)["serial"]; got != tc.want {
				t.Errorf("serial = %v, want %v", got, tc.want)
			}
		})
	}
}

// udapiKeys reports the udapi_caps bitmap and whether each of the two keys
// is present. The controller drops the whole capability update when the
// bitmap arrives without a version, so tests care about each separately.
func udapiKeys(t *testing.T, m map[string]any) (caps float64, hasCaps, hasVersion bool) {
	t.Helper()
	if raw, ok := m["udapi_caps"]; ok {
		hasCaps = true
		caps, ok = raw.(float64)
		if !ok {
			t.Fatalf("udapi_caps is not numeric: %v", m["udapi_caps"])
		}
	}
	if raw, ok := m["udapi_version"]; ok {
		hasVersion = true
		v, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("udapi_version is not an object: %v", raw)
		}
		if s, ok := v["version"].(string); !ok || s == "" {
			t.Errorf("udapi_version.version missing or empty: %v", v)
		}
	}
	return caps, hasCaps, hasVersion
}

// UNIFI_UDAPI_CAP_ROUTES_BGP, the one bit the catalog claims. Spelled out
// rather than read from the profile so a wrong regeneration fails here
// instead of agreeing with itself.
const udapiCapRoutesBGP = 1 << 22

// A real gateway reports its UDAPI capabilities on every inform, adopted
// or not, so the controller knows what it is before anyone adopts it.
func TestUXGEnterprisePayloadReportsUDAPICaps(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:27:22:e0:00:02", Model: "UXGENT", IP: "10.0.0.2"})
	for _, adopted := range []bool{false, true} {
		name := "pending"
		if adopted {
			markAdopted(d)
			name = "adopted"
		}
		t.Run(name, func(t *testing.T) {
			caps, hasCaps, hasVersion := udapiKeys(t, decodePayload(t, d))
			if !hasCaps || !hasVersion {
				t.Fatalf("udapi_caps present=%v, udapi_version present=%v; want both", hasCaps, hasVersion)
			}
			if caps != float64(udapiCapRoutesBGP) {
				t.Errorf("udapi_caps = %v, want %d (routes/BGP alone)", caps, udapiCapRoutesBGP)
			}
		})
	}
}

// Only UXG-Enterprise has UDAPI routing among the gateways that adopt by
// inform. Claiming it elsewhere makes the controller offer BGP against a
// device that answers 404 for it -- worse than not claiming it.
func TestOtherGatewaysReportNoUDAPICaps(t *testing.T) {
	for _, model := range []string{"UXG", "UXGB", "UXGA6AA", "UXGPRO", "UGW3", "UGW4", "UGWXG"} {
		t.Run(model, func(t *testing.T) {
			d := mustDevice(t, DeviceSpec{MAC: "00:27:22:e0:00:03", Model: model, IP: "10.0.0.3"})
			markAdopted(d)
			_, hasCaps, hasVersion := udapiKeys(t, decodePayload(t, d))
			if hasCaps || hasVersion {
				t.Errorf("udapi_caps present=%v, udapi_version present=%v; want neither", hasCaps, hasVersion)
			}
		})
	}
}

// fw_caps is keyed by firmware branch, so a model on a captured build
// reports the captured bitmap and one on any other build keeps the
// placeholder rather than borrowing a neighbour's.
func TestFWCapsFollowsTheFirmwareBranch(t *testing.T) {
	cases := []struct {
		model string
		want  int
		why   string
	}{
		{"U7PRO", -402850113, "uap on the captured 8.6.11.18870"},
		{"UGW3", 4378627, "ugw on the captured 4.4.57.5578372"},
		{"UGW4", 4378627, "same ugw firmware as UGW3"},
		{"UGWXG", placeholderFWCaps, "ugw on 4.4.57.5578378, which nobody captured"},
		{"U7MP", placeholderFWCaps, "uap on 6.8.2.15592, not the captured AP build"},
		{"UXGENT", placeholderFWCaps, "no uxg capture exists at all"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			d := mustDevice(t, DeviceSpec{MAC: "00:27:22:e0:00:31", Model: tc.model, IP: "10.0.0.31"})
			m := decodePayload(t, d)
			if got := m["fw_caps"]; got != float64(tc.want) {
				t.Errorf("fw_caps = %v, want %d (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// An explicit spec value beats the profile's, which is what let the live
// probe measure both arms against one model.
func TestFWCapsSpecOverrideWins(t *testing.T) {
	want := 12345
	d := mustDevice(t, DeviceSpec{
		MAC: "00:27:22:e0:00:32", Model: "U7PRO", IP: "10.0.0.32", FWCaps: &want,
	})
	if got := decodePayload(t, d)["fw_caps"]; got != float64(want) {
		t.Errorf("fw_caps = %v, want the spec override %d", got, want)
	}
	// Zero is a real claim, distinct from "unset", so it must survive.
	zero := 0
	d = mustDevice(t, DeviceSpec{
		MAC: "00:27:22:e0:00:33", Model: "U7PRO", IP: "10.0.0.33", FWCaps: &zero,
	})
	if got := decodePayload(t, d)["fw_caps"]; got != float64(0) {
		t.Errorf("fw_caps = %v, want an explicit 0 to survive as 0", got)
	}
}

func TestNewDeviceRejectsUnknownModel(t *testing.T) {
	if _, err := newDevice(DeviceSpec{MAC: "00:15:6d:00:00:09", Model: "NOPE"}, testInformURL); err == nil {
		t.Fatal("newDevice with unknown model: want error, got nil")
	}
}

// An explicit Type that contradicts the model profile builds an
// incoherent device (ugw identity with usw payload tables); reject it
// instead of letting a typo'd -type flag inform nonsense.
func TestNewDeviceRejectsMismatchedType(t *testing.T) {
	if _, err := newDevice(DeviceSpec{MAC: "00:15:6d:00:00:09", Model: "UGW3", Type: "usw"}, testInformURL); err == nil {
		t.Fatal("newDevice with Type usw on a ugw model: want error, got nil")
	}
	if _, err := newDevice(DeviceSpec{MAC: "00:15:6d:00:00:09", Model: "UGW3", Type: "ugw"}, testInformURL); err != nil {
		t.Fatalf("newDevice with matching explicit Type: %v", err)
	}
}

func TestDeviceSpecDefaults(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	if d.spec.Type != "uap" {
		t.Errorf("Type = %q, want profile default %q", d.spec.Type, "uap")
	}
	if d.spec.ModelDisplay == "" {
		t.Errorf("ModelDisplay not defaulted from profile")
	}
	// U7MP's real firmware version, harvested by cmd/modelgen from the
	// fw-update API; changes if the registry is regenerated against a newer
	// controller/firmware snapshot.
	if want := modelRegistry["U7MP"].Version; d.spec.Version != want {
		t.Errorf("Version = %q, want profile default %q", d.spec.Version, want)
	}
	if d.spec.Name != "UBNT" {
		t.Errorf("Name = %q, want default UBNT", d.spec.Name)
	}
	if d.session.AuthKey() != DefaultKey {
		t.Errorf("key = %q, want DefaultKey", d.session.AuthKey())
	}
	if d.interval != 10*time.Second {
		t.Errorf("interval = %v, want 10s", d.interval)
	}
	if d.state != StatePending {
		t.Errorf("state = %v, want PENDING", d.state)
	}
	if d.session.InformURL() != testInformURL {
		t.Errorf("informURL = %q, want %q", d.session.InformURL(), testInformURL)
	}

	// Type can only ever repeat the profile's (mismatches are rejected),
	// so the explicit-wins probe uses the other overridable fields.
	d2 := mustDevice(t, DeviceSpec{
		MAC: "00:15:6d:00:00:02", Model: "U7MP", IP: "10.0.0.58",
		Type: "uap", ModelDisplay: "Custom Display", Version: "9.9.9", Name: "ap1",
	})
	if d2.spec.Type != "uap" || d2.spec.ModelDisplay != "Custom Display" ||
		d2.spec.Version != "9.9.9" || d2.spec.Name != "ap1" {
		t.Errorf("explicit spec values did not win over profile defaults: %+v", d2.spec)
	}
}

func TestModelRegistryPayloads(t *testing.T) {
	for model, profile := range modelRegistry {
		t.Run(model, func(t *testing.T) {
			d := mustDevice(t, DeviceSpec{MAC: "02:00:00:00:00:01", Model: model, IP: "10.0.0.99"})
			markAdopted(d)
			m := decodePayload(t, d)
			// Both keys or neither, for every model of every type: a
			// device on firmware >= 4.1.0 that sends the bitmap without
			// a version has its whole capability update skipped, so the
			// bitmap is dropped and the device looks less capable than
			// one that claimed nothing.
			_, hasCaps, hasVersion := udapiKeys(t, m)
			if hasCaps != hasVersion {
				t.Errorf("%s: udapi_caps present=%v but udapi_version present=%v; the controller needs both or neither",
					model, hasCaps, hasVersion)
			}
			switch profile.Type {
			case "uap":
				table(t, m, "radio_table")
				// vaps default to empty until provisioned (see
				// TestAdoptedPayloadUAP); the key must still be present.
				if _, ok := m["vap_table"].([]any); !ok {
					t.Errorf("vap_table missing or not an array for uap model %s", model)
				}
			case "usw":
				table(t, m, "port_table")
				table(t, m, "ethernet_table")
			case "ugw", "uxg":
				stats, ok := m["system-stats"].(map[string]any)
				if !ok || len(stats) == 0 {
					t.Errorf("system-stats missing or empty for gateway model %s", model)
				}
			default:
				t.Errorf("profile %s has unknown type %q", model, profile.Type)
			}
		})
	}
}

func TestDeviceStateString(t *testing.T) {
	cases := map[DeviceState]string{
		StatePending:    "PENDING",
		StateAdopting:   "ADOPTING",
		StateConnected:  "CONNECTED",
		DeviceState(42): "UNKNOWN",
	}
	for state, want := range cases {
		if got := state.String(); got != want {
			t.Errorf("DeviceState(%d).String() = %q, want %q", int(state), got, want)
		}
	}
}
