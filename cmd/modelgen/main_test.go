package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const sampleEnvelope = `{
  "meta": {"rc": "ok"},
  "data": [
    {
      "model": "USW2",
      "type": "usw",
      "name": "Two-port switch",
      "version": "2.0.0",
      "state": 1,
      "adopted": true,
      "port_table": [
        {"ifname":"eth1","name":"LAN","port_idx":2,"media":"GE","poe_caps":7},
        {"ifname":"eth0","name":"WAN","port_idx":1,"media":"SFP+","is_uplink":true}
      ]
    },
    {
      "model": "UAP1",
      "type": "uap",
      "name": "One-radio AP",
      "version": "1.0.0",
      "state": 1,
      "adopted": true,
      "radio_table": [
        {
          "name":"wifi-ng",
          "radio":"ng",
          "channel":1,
          "ht":"20",
          "min_txpower":5,
          "max_txpower":20,
          "nss":2,
          "radio_caps":17,
          "antenna_gain":3
        }
      ],
      "port_table": [
        {"ifname":"eth0","name":"eth0","port_idx":1,"media":"GE","is_uplink":true}
      ]
    }
  ]
}`

const sampleDeviceDBBundle = `prefix({"UAP1":{"type":"uap","radios":{"ng":{"maxPower":20,"gain":3}}},"USW2":{"type":"usw","features":{"poe":true},"ports":{"standard":2,"plus":[2]}}})suffix`

func TestReduceProducesStableValidatedCatalog(t *testing.T) {
	catalog, err := reduce(strings.NewReader(sampleEnvelope), "10.4.57")
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if catalog.ControllerVersion != "10.4.57" {
		t.Fatalf("controller version = %q", catalog.ControllerVersion)
	}
	if len(catalog.Models) != 2 || catalog.Models[0].Model != "UAP1" ||
		catalog.Models[1].Model != "USW2" {
		t.Fatalf("models not sorted by ID: %+v", catalog.Models)
	}
	ports := catalog.Models[1].Ports
	if len(ports) != 2 || ports[0].PortIdx != 1 || ports[1].PortIdx != 2 {
		t.Fatalf("ports not sorted by index: %+v", ports)
	}
	if got := catalog.Models[0].Radios[0]; got.Radio != "ng" ||
		got.MaxTxPower != 20 || got.NSS != 2 || got.RadioCaps != 17 {
		t.Fatalf("radio facts lost: %+v", got)
	}

	var first, second bytes.Buffer
	if err := writeCatalog(&first, catalog); err != nil {
		t.Fatal(err)
	}
	if err := writeCatalog(&second, catalog); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatal("catalog output is not deterministic")
	}
	var decoded catalogFile
	if err := json.Unmarshal(first.Bytes(), &decoded); err != nil {
		t.Fatalf("catalog is invalid JSON: %v", err)
	}
}

func TestReduceRejectsDuplicateModels(t *testing.T) {
	raw := strings.Replace(sampleEnvelope, `"model": "USW2"`, `"model": "UAP1"`, 1)
	if _, err := reduce(strings.NewReader(raw), "10.4.57"); err == nil ||
		!strings.Contains(err.Error(), "duplicate model") {
		t.Fatalf("duplicate model error = %v", err)
	}
}

func TestReduceRejectsIncompleteDevices(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"not adopted": func(s string) string {
			return strings.Replace(s, `"adopted": true`, `"adopted": false`, 1)
		},
		"missing switch ports": func(s string) string {
			start := strings.Index(s, `"port_table": [`)
			end := strings.Index(s[start:], `]`) + start
			return s[:start] + `"port_table": []` + s[end+1:]
		},
		"duplicate port index": func(s string) string {
			return strings.Replace(s, `"port_idx":2`, `"port_idx":1`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := reduce(strings.NewReader(mutate(sampleEnvelope)), "10.4.57"); err == nil {
				t.Fatal("reduce accepted an incomplete device")
			}
		})
	}
}

func TestGenerateGoIsDeterministic(t *testing.T) {
	catalog, err := reduce(strings.NewReader(sampleEnvelope), "10.4.57")
	if err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	if err := writeGo(&first, catalog); err != nil {
		t.Fatal(err)
	}
	if err := writeGo(&second, catalog); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatal("Go output is not deterministic")
	}
	for _, want := range []string{
		`"UAP1": {`,
		`ModelDisplay: "One-radio AP"`,
		`PortIdx: 1`,
		`RadioCaps: 17`,
	} {
		if !strings.Contains(first.String(), want) {
			t.Errorf("generated Go missing %q:\n%s", want, first.String())
		}
	}
}

func TestReduceDeviceDatabaseMergesControllerIdentityAndHardwareFacts(t *testing.T) {
	pending := strings.ReplaceAll(sampleEnvelope, `"state": 1`, `"state": 2`)
	pending = strings.ReplaceAll(pending, `"adopted": true`, `"adopted": false`)
	catalog, err := reduceDeviceDatabase(
		strings.NewReader(pending),
		strings.NewReader(sampleDeviceDBBundle),
		"10.4.57",
	)
	if err != nil {
		t.Fatalf("reduceDeviceDatabase: %v", err)
	}
	ap, sw := catalog.Models[0], catalog.Models[1]
	if len(ap.Ports) != 1 || ap.Ports[0].IfName != "eth0" {
		t.Fatalf("AP ethernet ports = %+v, want one eth0", ap.Ports)
	}
	if len(ap.Radios) != 1 || ap.Radios[0].Radio != "ng" ||
		ap.Radios[0].MaxTxPower != 20 || ap.Radios[0].AntennaGain != 3 {
		t.Fatalf("AP radios = %+v, want controller hardware facts", ap.Radios)
	}
	if len(sw.Ports) != 2 || sw.Ports[0].Media != "GE" ||
		sw.Ports[1].Media != "SFP+" || sw.Ports[0].PoECaps != 7 {
		t.Fatalf("switch ports = %+v, want expanded media and PoE facts", sw.Ports)
	}
}

func TestReduceDeviceDatabaseRejectsMissingOrMismatchedModels(t *testing.T) {
	for name, bundle := range map[string]string{
		"missing":       `{"UAP1":{"type":"uap","radios":{"ng":{"maxPower":20}}}}`,
		"type mismatch": `{"UAP1":{"type":"usw","ports":{"standard":1}},"USW2":{"type":"usw","ports":{"standard":2}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := reduceDeviceDatabase(
				strings.NewReader(sampleEnvelope),
				strings.NewReader(bundle),
				"10.4.57",
			); err == nil {
				t.Fatal("reducer accepted incomplete controller metadata")
			}
		})
	}
}

func TestValidateModelRejectsInvalidHardwareFacts(t *testing.T) {
	base := catalogModel{
		Model: "UAP1", ModelDisplay: "AP", Type: "uap", Version: "1",
		Ports: []catalogPort{{
			IfName: "eth0", Name: "eth0", PortIdx: 1, Media: "GE", IsUplink: true,
		}},
		Radios: []catalogRadio{{
			Name: "wifi-ng", Radio: "ng", HT: "20",
			MinTxPower: 5, MaxTxPower: 20, NSS: 2,
		}},
	}
	for name, mutate := range map[string]func(*catalogModel){
		"no uplink": func(m *catalogModel) { m.Ports[0].IsUplink = false },
		"two uplinks": func(m *catalogModel) {
			m.Ports = append(m.Ports, catalogPort{
				IfName: "eth1", Name: "eth1", PortIdx: 2, Media: "GE", IsUplink: true,
			})
		},
		"unknown radio":           func(m *catalogModel) { m.Radios[0].Radio = "mystery" },
		"invalid radio power":     func(m *catalogModel) { m.Radios[0].MaxTxPower = 0 },
		"invalid spatial streams": func(m *catalogModel) { m.Radios[0].NSS = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			m := base
			m.Ports = append([]catalogPort(nil), base.Ports...)
			m.Radios = append([]catalogRadio(nil), base.Radios...)
			mutate(&m)
			if err := validateModel(&m); err == nil {
				t.Fatal("validateModel accepted invalid hardware facts")
			}
		})
	}
}
