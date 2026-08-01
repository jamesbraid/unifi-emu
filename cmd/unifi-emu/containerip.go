package main

import (
	"errors"
	"fmt"
	"net"
)

// netLister is the seam that makes address discovery testable: the process's
// interface list and the per-interface address lookup, injected so a table
// test can hand over a synthetic stack instead of needing a Docker network.
type netLister struct {
	interfaces func() ([]net.Interface, error)
	addrs      func(net.Interface) ([]net.Addr, error)
}

// realNet lists the addresses this process actually has.
func realNet() netLister {
	return netLister{
		interfaces: net.Interfaces,
		addrs:      func(i net.Interface) ([]net.Addr, error) { return i.Addrs() },
	}
}

// containerIPv4 returns the first non-loopback, non-link-local IPv4 address
// on an up interface — inside a container that is the address on its Docker
// network, which is the only address the controller can reach it on. A
// synthesized or auto-allocated address would be reported to the controller
// and stored as the device's IP, so anything reading stat/device (an
// operator, a provider under test) would be handed an address that answers
// nothing.
//
// Link-local (169.254/16) is skipped because it is what an interface has
// when DHCP has not answered yet: reporting it says "no network" in the one
// field meant to say where the device is. IPv6 is skipped because the
// controller's device IP field is an IPv4 one.
func containerIPv4(l netLister) (string, error) {
	ifaces, err := l.interfaces()
	if err != nil {
		return "", fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range ifaces {
		// A down interface's address is not reachable, and the loopback's
		// is not reachable from anywhere else.
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := l.addrs(iface)
		if err != nil {
			return "", fmt.Errorf("addresses of %s: %w", iface.Name, err)
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			return ip.String(), nil
		}
	}
	return "", errors.New("no non-loopback IPv4 address on any up interface")
}
