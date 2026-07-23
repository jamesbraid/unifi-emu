package emu

import (
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

const adoptKey = "aa03d5cfcc0d08eaf66e1cb798b07522"
const adoptURI = "http://172.30.0.2:8080/inform"

func applyAdopt(t *testing.T, d *device) {
	t.Helper()
	d.applyResponse([]byte(`{"_type":"cmd","cmd":"set-adopt","key":"` + adoptKey + `","uri":"` + adoptURI + `"}`))
	if !d.adopted {
		t.Fatal("set-adopt did not mark the device adopted")
	}
}

func TestSetAdoptRotatesKeyAndURL(t *testing.T) {
	for _, cmd := range []string{"set-adopt", "adopt"} {
		t.Run(cmd, func(t *testing.T) {
			d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
			d.applyResponse([]byte(`{"_type":"cmd","cmd":"` + cmd + `","key":"` + adoptKey + `","uri":"` + adoptURI + `"}`))

			if d.key != adoptKey {
				t.Errorf("key = %q, want rotated key %q", d.key, adoptKey)
			}
			if d.informURL != adoptURI {
				t.Errorf("informURL = %q, want %q", d.informURL, adoptURI)
			}
			if !d.adopted {
				t.Error("adopted = false after set-adopt")
			}
			if d.state != StateAdopting {
				t.Errorf("state = %v, want ADOPTING", d.state)
			}

			m := decodePayload(t, d)
			if m["x_authkey"] != adoptKey {
				t.Errorf("payload x_authkey = %v, want %q", m["x_authkey"], adoptKey)
			}
			if m["inform_url"] != adoptURI {
				t.Errorf("payload inform_url = %v, want %q", m["inform_url"], adoptURI)
			}
			if m["default"] != false || m["_default_key"] != false {
				t.Errorf("payload default flags = %v/%v, want false/false", m["default"], m["_default_key"])
			}
			if m["state"] != float64(2) {
				t.Errorf("payload state = %v, want 2", m["state"])
			}
		})
	}
}

// On the live -sim controller this is the ONLY key-delivery channel:
// set-adopt never arrives, the controller answers the first post-adopt
// inform with mgmt_cfg whose authkey equals the device doc's x_authkey
// (verified 2026-07-22: server.log "initial mgmt_cfg sent" on every
// inform, no set-adopt anywhere; tmp/itest/sim.log vs stat/device doc — gitignored live evidence).
// A device still on the default key must therefore adopt the mgmt_cfg
// authkey, exactly as if set-adopt had arrived.
func TestSetparamAdoptsAuthkeyFromMgmtCfg(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	d.applyResponse([]byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=4c36cd132e0a811601a3e0ca5793b677\n"}`))

	if d.key != "4c36cd132e0a811601a3e0ca5793b677" {
		t.Errorf("key = %q, want rotated to mgmt_cfg authkey", d.key)
	}
	if !d.adopted {
		t.Error("adopted = false after mgmt_cfg authkey rotation")
	}
	if d.state != StateAdopting {
		t.Errorf("state = %v, want ADOPTING", d.state)
	}
	if d.cfgvers != "abc123" {
		t.Errorf("cfgvers = %q, want abc123 from mgmt_cfg", d.cfgvers)
	}

	m := decodePayload(t, d)
	if m["x_authkey"] != "4c36cd132e0a811601a3e0ca5793b677" {
		t.Errorf("payload x_authkey = %v, want mgmt_cfg authkey", m["x_authkey"])
	}
	if m["default"] != false || m["_default_key"] != false {
		t.Errorf("payload default flags = %v/%v, want false/false", m["default"], m["_default_key"])
	}
	if m["cfgversion"] != "abc123" {
		t.Errorf("payload cfgversion = %v, want abc123", m["cfgversion"])
	}
}

// A pending device can see mgmt_cfg that still names the default key
// (unadopted provisioning). That is not adoption: no rotation, no state
// change — otherwise ordinary pre-adopt traffic would flip the device
// to ADOPTING and lie to the controller with default=false.
func TestSetparamDefaultAuthkeyKeepsPending(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	d.applyResponse([]byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=` + DefaultKey + `\n"}`))

	if d.key != DefaultKey {
		t.Errorf("key = %q, want DefaultKey unchanged", d.key)
	}
	if d.adopted {
		t.Error("adopted = true after default-key mgmt_cfg on a pending device")
	}
	if d.state != StatePending {
		t.Errorf("state = %v, want PENDING", d.state)
	}
	if d.cfgvers != "abc123" {
		t.Errorf("cfgvers = %q, want abc123 from mgmt_cfg", d.cfgvers)
	}
}

func TestSetparamAuthkeyIgnoredWhenAdopted(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	applyAdopt(t, d)
	d.applyResponse([]byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=def456\nauthkey=4c36cd132e0a811601a3e0ca5793b677\n"}`))

	if d.key != adoptKey {
		t.Errorf("key = %q, want the set-adopt key %q (mgmt_cfg.authkey must not clobber it)", d.key, adoptKey)
	}
	if d.cfgvers != "def456" {
		t.Errorf("cfgvers = %q, want def456", d.cfgvers)
	}
}

func TestSetparamTrimsMgmtCfgLines(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	d.applyResponse([]byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\r\nauthkey=` + DefaultKey + `\r\n"}`))

	if d.cfgvers != "abc123" {
		t.Errorf("cfgvers = %q, want exactly %q (CRLF must be trimmed)", d.cfgvers, "abc123")
	}
	if d.key != DefaultKey {
		t.Errorf("key = %q, want DefaultKey unchanged", d.key)
	}
}

// The two key-delivery paths are intentionally asymmetric: mgmt_cfg
// authkeys are gated (default key only, never clobber), but a set-adopt
// command is always authoritative and rotates unconditionally, even over
// a key adopted from mgmt_cfg. The -sim build never sends set-adopt, so
// this ordering is untested live; pin it so a future "gate set-adopt
// too" change is a deliberate decision, not an accident.
func TestSetAdoptOverridesMgmtCfgAdoptedKey(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	d.applyResponse([]byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=4c36cd132e0a811601a3e0ca5793b677\n"}`))
	if d.key != "4c36cd132e0a811601a3e0ca5793b677" || d.state != StateAdopting {
		t.Fatalf("mgmt_cfg adoption did not happen: key=%q state=%v", d.key, d.state)
	}

	d.applyResponse([]byte(`{"_type":"cmd","cmd":"set-adopt","key":"` + adoptKey + `","uri":"` + adoptURI + `"}`))

	if d.key != adoptKey {
		t.Errorf("key = %q, want set-adopt key %q (set-adopt is unconditional)", d.key, adoptKey)
	}
	if d.informURL != adoptURI {
		t.Errorf("informURL = %q, want %q", d.informURL, adoptURI)
	}
	if !d.adopted || d.state != StateAdopting {
		t.Errorf("adopted=%v state=%v, want adopted and ADOPTING", d.adopted, d.state)
	}
}

// The controller resends the same mgmt_cfg on every inform until the
// device acknowledges provisioning. Once the authkey is adopted, repeats
// must be inert: re-rotating would flap the state back to ADOPTING after
// the device already reached CONNECTED.
func TestSetparamSameAuthkeyDoesNotReadopt(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	cfg := `{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=4c36cd132e0a811601a3e0ca5793b677\n"}`
	d.applyResponse([]byte(cfg))
	d.state = StateConnected // the inform loop flips this after the handshake
	d.applyResponse([]byte(cfg))
	d.applyResponse([]byte(cfg))

	if d.key != "4c36cd132e0a811601a3e0ca5793b677" {
		t.Errorf("key = %q, want unchanged mgmt_cfg authkey", d.key)
	}
	if d.state != StateConnected {
		t.Errorf("state = %v, want CONNECTED undisturbed by mgmt_cfg repeats", d.state)
	}
}

func TestNoopSetsInterval(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	d.applyResponse([]byte(`{"_type":"noop","interval":30}`))
	if d.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", d.interval)
	}
	d.applyResponse([]byte(`{"_type":"noop","interval":0}`))
	if d.interval != 30*time.Second {
		t.Errorf("interval = %v after interval:0, want unchanged 30s", d.interval)
	}
}

// The controller holds an upgrading device in state=4 until an inform
// arrives carrying the target version, so the emulated upgrade must swap
// the version and restart uptime the way a flash-and-reboot would.
func TestUpgradeAppliesVersionAndReboots(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7PRO", IP: "10.0.0.57"})
	applyAdopt(t, d)
	before := d.started
	time.Sleep(time.Millisecond) // make a started-reset observable
	d.applyResponse([]byte(`{"_type":"upgrade","version":"8.6.11.18870","md5sum":"f300eb4ad0732161949b6cb06b0c6858","url":"https://fw-download.ubnt.com/data/unifi-firmware/1718-U7PRO-8.6.11-94862a62-887a-4433-b148-3dcfc93e672f.bin"}`))

	if d.spec.Version != "8.6.11.18870" {
		t.Errorf("version = %q, want upgraded 8.6.11.18870", d.spec.Version)
	}
	if !d.started.After(before) {
		t.Error("started not reset; emulated reboot must restart uptime")
	}
	if m := decodePayload(t, d); m["version"] != "8.6.11.18870" {
		t.Errorf("payload version = %v, want 8.6.11.18870", m["version"])
	}

	// An upgrade is a firmware swap, not a reset: adoption state must
	// survive it unchanged, or the device would fall back to pending
	// after every controller-requested upgrade.
	if d.key != adoptKey {
		t.Errorf("key changed across upgrade, want rotated key preserved")
	}
	if !d.adopted {
		t.Error("adopted = false after upgrade, want unchanged")
	}
	if d.state != StateAdopting {
		t.Errorf("state = %v after upgrade, want unchanged ADOPTING", d.state)
	}

	// A version-less upgrade must not clobber the current version.
	d.applyResponse([]byte(`{"_type":"upgrade"}`))
	if d.spec.Version != "8.6.11.18870" {
		t.Errorf("version = %q after version-less upgrade, want unchanged 8.6.11.18870", d.spec.Version)
	}

	// A factory reset wipes adoption but not flashed firmware: the
	// upgraded version survives setdefault.
	d.applyResponse([]byte(`{"_type":"cmd","cmd":"setdefault"}`))
	if d.spec.Version != "8.6.11.18870" {
		t.Errorf("version = %q after setdefault, want no downgrade from 8.6.11.18870", d.spec.Version)
	}
}

func TestSetstateEchoesConfig(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	applyAdopt(t, d)
	d.applyResponse([]byte(`{"_type":"setstate","cfgversion":"beef42","vap_table":[{"essid":"CorpWiFi","bssid":"00:15:6d:00:00:11","radio":"ng","up":true}]}`))

	if d.cfgvers != "beef42" {
		t.Errorf("cfgvers = %q, want beef42", d.cfgvers)
	}
	m := decodePayload(t, d)
	if m["cfgversion"] != "beef42" {
		t.Errorf("payload cfgversion = %v, want beef42", m["cfgversion"])
	}
	vap, ok := table(t, m, "vap_table")[0].(map[string]any)
	if !ok {
		t.Fatalf("vap_table[0] not an object")
	}
	if vap["essid"] != "CorpWiFi" {
		t.Errorf("echoed vap_table[0].essid = %v, want CorpWiFi", vap["essid"])
	}
}

func TestSetdefaultResetsToPending(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	applyAdopt(t, d)
	d.applyResponse([]byte(`{"_type":"setstate","cfgversion":"beef42","vap_table":[{"essid":"CorpWiFi"}]}`))
	d.applyResponse([]byte(`{"_type":"cmd","cmd":"setdefault"}`))

	if d.adopted {
		t.Error("adopted = true after setdefault")
	}
	if d.key != DefaultKey {
		t.Errorf("key = %q, want DefaultKey after setdefault", d.key)
	}
	if d.cfgvers != "0" {
		t.Errorf("cfgvers = %q, want \"0\" after setdefault", d.cfgvers)
	}
	if d.state != StatePending {
		t.Errorf("state = %v, want PENDING", d.state)
	}
	if d.setstate != nil {
		t.Errorf("setstate = %v, want nil after setdefault", d.setstate)
	}

	m := decodePayload(t, d)
	if m["default"] != true || m["state"] != float64(1) || m["x_authkey"] != DefaultKey {
		t.Errorf("payload not back to pending shape: default=%v state=%v x_authkey=%v",
			m["default"], m["state"], m["x_authkey"])
	}

	// Re-adopt and confirm the cleared setstate no longer echoes: the
	// vap_table must be back to the empty default, not the pushed config.
	markAdopted(d)
	m = decodePayload(t, d)
	vaps, _ := m["vap_table"].([]any)
	for _, e := range vaps {
		if e.(map[string]any)["essid"] == "CorpWiFi" {
			t.Error("stale setstate vap_table still echoed after setdefault")
		}
	}
}

// mgmt_cfg carries the controller's provisioning handshake (authkey,
// cfgversion), and on some controller builds it is the only adoption
// delivery channel — but it can arrive on every inform, so log it only
// when its content changes: first sight and every change, never repeats.
func TestSetparamLogsMgmtCfgTransitions(t *testing.T) {
	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	cfg := `{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=4c36cd132e0a811601a3e0ca5793b677\n"}`
	d.applyResponse([]byte(cfg))
	if !strings.Contains(logs.String(), "authkey=4c36cd132e0a811601a3e0ca5793b677") {
		t.Fatalf("first mgmt_cfg not logged, got %q", logs.String())
	}
	d.applyResponse([]byte(cfg)) // identical repeat: must stay silent
	if n := strings.Count(logs.String(), ": mgmt_cfg: "); n != 1 {
		t.Fatalf("identical mgmt_cfg logged %d times, want 1: %q", n, logs.String())
	}
	d.applyResponse([]byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=def456\nauthkey=4c36cd132e0a811601a3e0ca5793b677\n"}`))
	if n := strings.Count(logs.String(), ": mgmt_cfg: "); n != 2 {
		t.Fatalf("changed mgmt_cfg logged %d times total, want 2: %q", n, logs.String())
	}
}

func TestUnknownResponseIgnored(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	for _, body := range []string{
		`{"_type":"wibble","interval":99,"key":"zzz"}`,
		`{"_type":"cmd","cmd":"reboot"}`,
		`this is not json`,
		``,
	} {
		d.applyResponse([]byte(body))
	}
	if d.adopted || d.key != DefaultKey || d.cfgvers != "0" ||
		d.state != StatePending || d.interval != 10*time.Second {
		t.Errorf("unknown responses mutated device state: adopted=%v key=%q cfgvers=%q state=%v interval=%v",
			d.adopted, d.key, d.cfgvers, d.state, d.interval)
	}
}
