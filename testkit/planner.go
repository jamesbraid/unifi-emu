package testkit

import (
	"errors"
	"fmt"
	"net"
	"strings"

	emu "github.com/jamesbraid/unifi-emu"
)

type ContainerPlan struct {
	RuntimeName string // "synthetic" or the custom runtime name
	Image       string // OCI image name, or empty for synthetic (handled by harness)
	CapAdd      []string
	Isolation   string       // "synthetic", "fleet", "device"
	Specs       []emu.DeviceSpec
	LogName     string
}

type LaunchPlan struct {
	Containers []ContainerPlan
}

func ExpandFleet(specs []emu.DeviceSpec, cat *Catalog, macBase, ipBase string) ([]emu.DeviceSpec, error) {
	baseMAC, err := net.ParseMAC(macBase)
	if err != nil {
		return nil, fmt.Errorf("SIM_MAC_BASE %q: %w", macBase, err)
	}
	baseIP := net.ParseIP(ipBase).To4()
	if baseIP == nil {
		return nil, fmt.Errorf("SIM_IP_BASE %q: not an IPv4 address", ipBase)
	}
	out := make([]emu.DeviceSpec, len(specs))
	gateways := 0
	for i, s := range specs {
		isExternal := cat.RuntimeFor(s.Model) != nil

		if !isExternal {
			profile, ok := emu.Profile(s.Model)
			if !ok {
				return nil, fmt.Errorf("unknown model %q", s.Model)
			}
			if profile.Type == "ugw" || profile.Type == "uxg" {
				gateways++
				if gateways > 1 {
					return nil, fmt.Errorf("fleet has %d gateways; a site adopts at most one (api.err.NoSecondGateway)", gateways)
				}
			}
			if s.Type == "" {
				s.Type = profile.Type
			}
			if s.ModelDisplay == "" {
				s.ModelDisplay = profile.ModelDisplay
			}
			if s.Version == "" {
				s.Version = profile.Version
			}
		}

		if s.MAC == "" {
			s.MAC = emu.NextMAC(baseMAC, i+1)
		}
		if s.IP == "" {
			ip, err := emu.NextIP(baseIP, i)
			if err != nil {
				return nil, err
			}
			s.IP = ip
		}
		if s.Serial == "" {
			s.Serial = strings.ToUpper(strings.ReplaceAll(s.MAC, ":", ""))
		}
		if s.Name == "" {
			s.Name = "UBNT"
		}
		out[i] = s
	}
	return out, nil
}

func PlanLaunch(specs []emu.DeviceSpec, cat *Catalog) (*LaunchPlan, error) {
	if len(specs) == 0 {
		return nil, errors.New("empty fleet: no devices to launch")
	}

	var plan LaunchPlan
	type key struct {
		runtime string
		isFleet bool
	}
	planMap := make(map[key]int)

	for _, spec := range specs {
		matched := cat.RuntimeFor(spec.Model)

		if matched == nil {
			// Synthetic
			k := key{runtime: "synthetic", isFleet: true}
			idx, ok := planMap[k]
			if !ok {
				newCp := ContainerPlan{
					RuntimeName: "synthetic",
					Isolation:   "synthetic",
					Specs:       []emu.DeviceSpec{},
				}
				plan.Containers = append(plan.Containers, newCp)
				idx = len(plan.Containers) - 1
				planMap[k] = idx
			}
			plan.Containers[idx].Specs = append(plan.Containers[idx].Specs, spec)
		} else if matched.Isolation == "fleet" {
			k := key{runtime: matched.Name, isFleet: true}
			idx, ok := planMap[k]
			if !ok {
				newCp := ContainerPlan{
					RuntimeName: matched.Name,
					Image:       matched.Image,
					CapAdd:      matched.CapAdd,
					Isolation:   "fleet",
					Specs:       []emu.DeviceSpec{},
				}
				plan.Containers = append(plan.Containers, newCp)
				idx = len(plan.Containers) - 1
				planMap[k] = idx
			}
			plan.Containers[idx].Specs = append(plan.Containers[idx].Specs, spec)
		} else {
			// device-isolated: always a new container per device
			newCp := ContainerPlan{
				RuntimeName: matched.Name,
				Image:       matched.Image,
				CapAdd:      matched.CapAdd,
				Isolation:   "device",
				Specs:       []emu.DeviceSpec{spec},
			}
			plan.Containers = append(plan.Containers, newCp)
		}
	}

	// Now set the LogName for each planned container.
	onlyOneSynthetic := len(plan.Containers) == 1 && plan.Containers[0].RuntimeName == "synthetic"
	for i := range plan.Containers {
		cp := &plan.Containers[i]
		if cp.RuntimeName == "synthetic" && onlyOneSynthetic {
			cp.LogName = "emulator.log"
		} else if cp.RuntimeName == "synthetic" {
			cp.LogName = "emulator-synthetic.log"
		} else if cp.Isolation == "fleet" {
			cp.LogName = fmt.Sprintf("emulator-%s.log", cp.RuntimeName)
		} else {
			macClean := strings.ReplaceAll(strings.ToLower(cp.Specs[0].MAC), ":", "")
			cp.LogName = fmt.Sprintf("emulator-%s-%s.log", cp.RuntimeName, macClean)
		}
	}

	return &plan, nil
}
