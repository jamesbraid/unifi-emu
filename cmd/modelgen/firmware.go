package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// firmwareIndex maps a model code to a device-format firmware version.
type firmwareIndex map[string]string

// perTypeFirmwareDefault is the fallback for models the API doesn't list
// (EOL or too-new). Values are the observed release trains per type.
var perTypeFirmwareDefault = map[string]string{
	"ugw": "4.4.57.5578372",
	"usw": "7.3.109.16640",
	"uap": "8.6.11.18870",
}

type fwResponse struct {
	Embedded struct {
		Firmware []struct {
			Platform string `json:"platform"`
			Product  string `json:"product"`
			Version  string `json:"version"`
		} `json:"firmware"`
	} `json:"_embedded"`
}

// parseFirmware reads a firmware-latest response and indexes network gear by
// model code. For unifi-firmware entries the API's `platform` field is the
// model code; the version (v4.4.57+5578372) is transformed to the
// device-reported form (4.4.57.5578372).
func parseFirmware(r io.Reader) (firmwareIndex, error) {
	var resp fwResponse
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode firmware-latest: %w", err)
	}
	idx := make(firmwareIndex, len(resp.Embedded.Firmware))
	for _, f := range resp.Embedded.Firmware {
		if f.Product != "unifi-firmware" || f.Platform == "" || f.Version == "" {
			continue
		}
		idx[f.Platform] = transformFirmwareVersion(f.Version)
	}
	return idx, nil
}

func transformFirmwareVersion(v string) string {
	return strings.Replace(strings.TrimPrefix(v, "v"), "+", ".", 1)
}

// firmwareVersion returns the model's real version, or the per-type default.
func firmwareVersion(idx firmwareIndex, model, typ string) string {
	if v, ok := idx[model]; ok {
		return v
	}
	return perTypeFirmwareDefault[typ]
}
