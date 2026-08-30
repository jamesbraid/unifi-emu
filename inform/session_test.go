package inform

import (
	"encoding/json"
	"testing"
	"time"
)

const (
	adoptKey = "aa03d5cfcc0d08eaf66e1cb798b07522"
	adoptURI = "http://172.30.0.2:8080/inform"
)

func TestSetAdoptRotatesKeyAndURL(t *testing.T) {
	for _, cmd := range []string{"set-adopt", "adopt"} {
		t.Run(cmd, func(t *testing.T) {
			s := NewSession(uapDesc(), testInformURL, testClock)
			s.Apply(testClock, []byte(`{"_type":"cmd","cmd":"`+cmd+`","key":"`+adoptKey+`","uri":"`+adoptURI+`"}`))
			if s.AuthKey() != adoptKey {
				t.Errorf("key = %q, want %q", s.AuthKey(), adoptKey)
			}
			if s.InformURL() != adoptURI {
				t.Errorf("informURL = %q, want %q", s.InformURL(), adoptURI)
			}
			if !s.Adopted() {
				t.Error("adopted = false after set-adopt")
			}
			m := decode(t, s)
			if m["x_authkey"] != adoptKey || m["inform_url"] != adoptURI {
				t.Errorf("payload not carrying the rotated key/url: %v / %v", m["x_authkey"], m["inform_url"])
			}
			if m["default"] != false || m["state"] != float64(4) {
				t.Errorf("payload not in adopted shape: default=%v state=%v", m["default"], m["state"])
			}
		})
	}
}

func TestSetAdoptEmitsAdoptingEffect(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	fx := s.Apply(testClock, []byte(`{"_type":"cmd","cmd":"set-adopt","uri":"`+adoptURI+`"}`))
	if len(fx) != 1 || fx[0].Kind != EffectAdoptingViaSetAdopt || fx[0].Text != adoptURI {
		t.Fatalf("effects = %+v, want one EffectAdoptingViaSetAdopt with the uri", fx)
	}
}

func TestSetparamAdoptsAuthkeyFromMgmtCfg(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	fx := s.Apply(testClock, []byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=4c36cd132e0a811601a3e0ca5793b677\nuse_aes_gcm=true\n"}`))
	if s.AuthKey() != "4c36cd132e0a811601a3e0ca5793b677" || !s.Adopted() {
		t.Errorf("mgmt_cfg authkey not adopted: key=%q adopted=%v", s.AuthKey(), s.Adopted())
	}
	if !s.UseAESGCM() {
		t.Error("use_aes_gcm=true not applied")
	}
	// The mgmt_cfg body is reported first, then the adopting effect.
	if len(fx) != 2 || fx[0].Kind != EffectMgmtCfg || fx[1].Kind != EffectAdoptingViaMgmtCfg {
		t.Fatalf("effects = %+v, want [MgmtCfg, AdoptingViaMgmtCfg]", fx)
	}
}

func TestSetparamDefaultAuthkeyKeepsPending(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	s.Apply(testClock, []byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=`+DefaultKey+`\n"}`))
	if s.AuthKey() != DefaultKey || s.Adopted() {
		t.Errorf("default-key mgmt_cfg must not adopt: key=%q adopted=%v", s.AuthKey(), s.Adopted())
	}
}

func TestSetparamAuthkeyIgnoredWhenAdopted(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	s.Apply(testClock, []byte(`{"_type":"cmd","cmd":"set-adopt","key":"`+adoptKey+`"}`))
	s.Apply(testClock, []byte(`{"_type":"setparam","mgmt_cfg":"authkey=4c36cd132e0a811601a3e0ca5793b677\n"}`))
	if s.AuthKey() != adoptKey {
		t.Errorf("key = %q, want the set-adopt key preserved (mgmt_cfg must not clobber it)", s.AuthKey())
	}
}

func TestSetAdoptOverridesMgmtCfgAdoptedKey(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	s.Apply(testClock, []byte(`{"_type":"setparam","mgmt_cfg":"authkey=4c36cd132e0a811601a3e0ca5793b677\n"}`))
	s.Apply(testClock, []byte(`{"_type":"cmd","cmd":"set-adopt","key":"`+adoptKey+`","uri":"`+adoptURI+`"}`))
	if s.AuthKey() != adoptKey {
		t.Errorf("key = %q, want set-adopt to override the mgmt_cfg key unconditionally", s.AuthKey())
	}
}

func TestNoopSetsInterval(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	fx := s.Apply(testClock, []byte(`{"_type":"noop","interval":30}`))
	if len(fx) != 1 || fx[0].Kind != EffectInterval || fx[0].Interval != 30*time.Second {
		t.Fatalf("effects = %+v, want one EffectInterval of 30s", fx)
	}
	if fx := s.Apply(testClock, []byte(`{"_type":"noop","interval":0}`)); len(fx) != 0 {
		t.Errorf("interval:0 produced %+v, want no effect", fx)
	}
}

func TestUpgradeAppliesVersionAndReboots(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	s.Apply(testClock, []byte(`{"_type":"cmd","cmd":"set-adopt","key":"`+adoptKey+`"}`))
	later := testClock.Add(time.Hour)
	s.Apply(later, []byte(`{"_type":"upgrade","version":"8.6.11.18870"}`))

	if m := decode(t, s); m["version"] != "8.6.11.18870" {
		t.Errorf("payload version = %v, want the upgraded version", m["version"])
	}
	// Adoption survives an upgrade (firmware swap, not reset).
	if s.AuthKey() != adoptKey || !s.Adopted() {
		t.Errorf("upgrade disturbed adoption: key=%q adopted=%v", s.AuthKey(), s.Adopted())
	}
	// bootTime reset: uptime measured from `later` is ~0, not an hour.
	var m map[string]any
	if err := json.Unmarshal(s.BuildPayload(later), &m); err != nil {
		t.Fatalf("BuildPayload is not valid JSON: %v", err)
	}
	if up := m["uptime"].(float64); up != 0 {
		t.Errorf("uptime = %v after upgrade at the same instant, want 0 (bootTime reset)", up)
	}
	// A version-less upgrade must not clobber the version.
	s.Apply(later, []byte(`{"_type":"upgrade"}`))
	if m := decode(t, s); m["version"] != "8.6.11.18870" {
		t.Errorf("version-less upgrade changed version to %v", m["version"])
	}
}

func TestSetstateEchoesConfig(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	s.Apply(testClock, []byte(`{"_type":"cmd","cmd":"set-adopt","key":"`+adoptKey+`"}`))
	s.Apply(testClock, []byte(`{"_type":"setstate","cfgversion":"beef42","vap_table":[{"essid":"CorpWiFi","bssid":"00:15:6d:00:00:11","radio":"ng","up":true}]}`))
	m := decode(t, s)
	if m["cfgversion"] != "beef42" {
		t.Errorf("cfgversion = %v, want beef42", m["cfgversion"])
	}
	vaps := m["vap_table"].([]any)
	if vaps[0].(map[string]any)["essid"] != "CorpWiFi" {
		t.Errorf("echoed vap essid = %v, want CorpWiFi", vaps[0].(map[string]any)["essid"])
	}
}

func TestSetdefaultResetsToPending(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	s.Apply(testClock, []byte(`{"_type":"cmd","cmd":"set-adopt","key":"`+adoptKey+`"}`))
	s.Apply(testClock, []byte(`{"_type":"setstate","cfgversion":"beef42","vap_table":[{"essid":"CorpWiFi"}]}`))
	fx := s.Apply(testClock, []byte(`{"_type":"cmd","cmd":"setdefault"}`))
	if s.Adopted() || s.AuthKey() != DefaultKey {
		t.Errorf("setdefault did not reset: adopted=%v key=%q", s.Adopted(), s.AuthKey())
	}
	if len(fx) != 1 || fx[0].Kind != EffectFactoryReset {
		t.Fatalf("effects = %+v, want one EffectFactoryReset", fx)
	}
	m := decode(t, s)
	if m["default"] != true || m["x_authkey"] != DefaultKey {
		t.Errorf("payload not back to pending: default=%v x_authkey=%v", m["default"], m["x_authkey"])
	}
	if _, ok := m["vap_table"]; ok {
		t.Error("stale setstate still echoed after setdefault (pending payload has no vap_table)")
	}
}

func TestUnknownResponseIgnored(t *testing.T) {
	s := NewSession(uapDesc(), testInformURL, testClock)
	for _, body := range []string{`{"_type":"wibble","key":"zzz"}`, `this is not json`, ``} {
		s.Apply(testClock, []byte(body))
	}
	if s.Adopted() || s.AuthKey() != DefaultKey {
		t.Errorf("unknown responses mutated state: adopted=%v key=%q", s.Adopted(), s.AuthKey())
	}
}
