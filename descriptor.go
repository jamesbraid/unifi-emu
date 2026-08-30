package emu

import (
	"fmt"
	"strings"

	"github.com/jamesbraid/unifi-emu/inform"
)

// buildDescriptor resolves a spec + model profile into the immutable identity
// and shape the inform payload reports. All defaulting and overrides happen
// here so the inform package stays a pure reporter.
func buildDescriptor(spec DeviceSpec, profile ModelProfile) inform.Descriptor {
	return inform.Descriptor{
		MAC:          spec.MAC,
		Serial:       resolveSerial(spec),
		Model:        spec.Model,
		ModelDisplay: spec.ModelDisplay,
		Version:      spec.Version,
		IP:           spec.IP,
		Hostname:     spec.Name,
		Type:         profile.Type,
		FWCaps:       resolveFWCaps(spec, profile),
		UDAPIVersion: profile.UDAPIVersion,
		UDAPICaps:    profile.UDAPICaps,
		Ports:        resolvePorts(spec, profile),
		Radios:       profile.Radios,
		SSIDs:        spec.SSIDs,
	}
}

// resolveSerial reports the spec's serial when set, else the MAC in the
// uppercase separator-less form real devices use.
func resolveSerial(spec DeviceSpec) string {
	if spec.Serial != "" {
		return spec.Serial
	}
	return strings.ToUpper(strings.ReplaceAll(spec.MAC, ":", ""))
}

// resolveFWCaps applies the precedence: explicit spec value beats the model's
// captured bitmap, which beats the placeholder.
func resolveFWCaps(spec DeviceSpec, profile ModelProfile) int {
	switch {
	case spec.FWCaps != nil:
		return *spec.FWCaps
	case profile.FWCaps != 0:
		return profile.FWCaps
	default:
		return inform.PlaceholderFWCaps
	}
}

// resolvePorts returns the spec port-count override synthesized in the
// profile's style, or the profile layout when no override is set.
func resolvePorts(spec DeviceSpec, profile ModelProfile) []inform.Port {
	if spec.Ports <= 0 {
		return profile.Ports
	}
	ports := make([]inform.Port, 0, spec.Ports)
	for i := 1; i <= spec.Ports; i++ {
		ports = append(ports, inform.Port{
			IfName:   fmt.Sprintf("eth%d", i-1),
			Name:     fmt.Sprintf("Port %d", i),
			PortIdx:  i,
			Media:    "GE",
			IsUplink: i == 1,
		})
	}
	return ports
}
