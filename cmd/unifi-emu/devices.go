package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jamesbraid/unifi-emu"
	"gopkg.in/yaml.v3"
)

// singleFlags are the flags that build one device. Combined with a fleet
// source, one of the two definitions would silently drop, so the
// combination is either rejected (-devices) or the flags lose
// (SIM_DEVICES).
var singleFlags = []string{"mac", "type", "model", "model-display", "version", "name", "ip"}

// fleetSpecs resolves the device list from the fleet sources, in
// precedence order:
//
//	-devices FILE > SIM_DEVICES env > single-device flags
//
// set marks the flags explicitly given on the command line. Both fleet
// sources at once are ambiguous and an error. Single-device flags
// combined with -devices are an error; combined with SIM_DEVICES they
// lose, and the losing flag names are returned so the caller can log the
// override. A nil spec slice means single-device mode.
func fleetSpecs(devicesFile, envInline string, set map[string]bool) ([]emu.DeviceSpec, []string, error) {
	if devicesFile != "" && strings.TrimSpace(envInline) != "" {
		return nil, nil, errors.New("ambiguous device sources: -devices and SIM_DEVICES are both set")
	}
	if devicesFile != "" {
		for _, f := range singleFlags {
			if set[f] {
				return nil, nil, fmt.Errorf("-devices cannot be combined with -%s", f)
			}
		}
	}
	specs, err := loadDevices(devicesFile, envInline)
	if err != nil {
		return nil, nil, err
	}
	if specs == nil || devicesFile != "" {
		return specs, nil, nil
	}
	var ignored []string
	for _, f := range singleFlags {
		if set[f] {
			ignored = append(ignored, f)
		}
	}
	return specs, ignored, nil
}

// loadDevices reads the fleet from filePath or, when that is empty, from
// the SIM_DEVICES env value envInline. Both hold a YAML list of
// DeviceSpec; JSON files keep working because JSON is a YAML subset.
// Neither set returns (nil, nil): the caller falls back to the
// single-device flags.
func loadDevices(filePath, envInline string) ([]emu.DeviceSpec, error) {
	var src string
	var b []byte
	switch {
	case filePath != "":
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filePath, err)
		}
		src, b = filePath, data
	case strings.TrimSpace(envInline) != "":
		src, b = "SIM_DEVICES", []byte(envInline)
	default:
		return nil, nil
	}
	// KnownFields keeps the strictness the JSON-only loader had with
	// DisallowUnknownFields: a misspelled DeviceSpec key (modle) must
	// fail loudly, not silently drop to the profile default.
	var specs []emu.DeviceSpec
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&specs); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", src, err)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("%s: no devices", src)
	}
	return specs, nil
}
