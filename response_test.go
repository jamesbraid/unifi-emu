package unifiemu

import (
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

func TestSetparamIgnoresMgmtCfgAuthkey(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	d.applyResponse([]byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=4c36cd132e0a811601a3e0ca5793b677\n"}`))

	if d.key != DefaultKey {
		t.Errorf("key = %q, want DefaultKey unchanged (mgmt_cfg.authkey must be ignored)", d.key)
	}
	if d.cfgvers != "abc123" {
		t.Errorf("cfgvers = %q, want abc123 from mgmt_cfg", d.cfgvers)
	}
	if d.adopted {
		t.Error("adopted = true after setparam on a pending device")
	}
	if d.state != StatePending {
		t.Errorf("state = %v, want PENDING", d.state)
	}

	m := decodePayload(t, d)
	if m["x_authkey"] != DefaultKey {
		t.Errorf("payload x_authkey = %v, want DefaultKey", m["x_authkey"])
	}
	if m["cfgversion"] != "abc123" {
		t.Errorf("payload cfgversion = %v, want abc123", m["cfgversion"])
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
	d.applyResponse([]byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\r\nauthkey=4c36cd132e0a811601a3e0ca5793b677\r\n"}`))

	if d.cfgvers != "abc123" {
		t.Errorf("cfgvers = %q, want exactly %q (CRLF must be trimmed)", d.cfgvers, "abc123")
	}
	if d.key != DefaultKey {
		t.Errorf("key = %q, want DefaultKey unchanged (mgmt_cfg.authkey must be ignored)", d.key)
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
	// vap_table must be the profile default again, not the pushed config.
	markAdopted(d)
	m = decodePayload(t, d)
	for _, e := range table(t, m, "vap_table") {
		if e.(map[string]any)["essid"] == "CorpWiFi" {
			t.Error("stale setstate vap_table still echoed after setdefault")
		}
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
