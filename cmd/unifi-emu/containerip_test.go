package main

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// fakeNet builds a lister over a synthetic interface list, so the picker is
// tested without a Docker network under it. addrs maps interface name to the
// addresses that interface reports.
func fakeNet(ifaces []net.Interface, addrs map[string][]net.Addr) netLister {
	return netLister{
		interfaces: func() ([]net.Interface, error) { return ifaces, nil },
		addrs:      func(i net.Interface) ([]net.Addr, error) { return addrs[i.Name], nil },
	}
}

func cidr(t *testing.T, s string) net.Addr {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return &net.IPNet{IP: ip, Mask: n.Mask}
}

func TestContainerIPv4(t *testing.T) {
	up := net.FlagUp | net.FlagRunning
	lo := net.Interface{Index: 1, Name: "lo", Flags: up | net.FlagLoopback}
	eth0 := net.Interface{Index: 2, Name: "eth0", Flags: up}
	eth1 := net.Interface{Index: 3, Name: "eth1", Flags: up}
	down := net.Interface{Index: 4, Name: "eth9"}

	for name, tc := range map[string]struct {
		ifaces []net.Interface
		addrs  map[string][]net.Addr
		want   string
	}{
		"single address": {
			[]net.Interface{eth0},
			map[string][]net.Addr{"eth0": {cidr(t, "172.18.0.5/16")}},
			"172.18.0.5",
		},
		"skips loopback": {
			[]net.Interface{lo, eth0},
			map[string][]net.Addr{
				"lo":   {cidr(t, "127.0.0.1/8")},
				"eth0": {cidr(t, "172.18.0.5/16")},
			},
			"172.18.0.5",
		},
		"skips link-local": {
			[]net.Interface{eth0, eth1},
			map[string][]net.Addr{
				"eth0": {cidr(t, "169.254.7.7/16")},
				"eth1": {cidr(t, "172.18.0.5/16")},
			},
			"172.18.0.5",
		},
		"skips ipv6": {
			[]net.Interface{eth0},
			map[string][]net.Addr{"eth0": {
				cidr(t, "fe80::1/64"), cidr(t, "2001:db8::1/64"), cidr(t, "172.18.0.5/16"),
			}},
			"172.18.0.5",
		},
		"skips down interfaces": {
			[]net.Interface{down, eth0},
			map[string][]net.Addr{
				"eth9": {cidr(t, "10.9.9.9/24")},
				"eth0": {cidr(t, "172.18.0.5/16")},
			},
			"172.18.0.5",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := containerIPv4(fakeNet(tc.ifaces, tc.addrs))
			if err != nil {
				t.Fatalf("containerIPv4: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestContainerIPv4NoneFound(t *testing.T) {
	up := net.FlagUp | net.FlagRunning
	ifaces := []net.Interface{
		{Index: 1, Name: "lo", Flags: up | net.FlagLoopback},
		{Index: 2, Name: "eth0", Flags: up},
	}
	addrs := map[string][]net.Addr{
		"lo":   {cidr(t, "127.0.0.1/8")},
		"eth0": {cidr(t, "169.254.7.7/16"), cidr(t, "fe80::1/64")},
	}
	_, err := containerIPv4(fakeNet(ifaces, addrs))
	if err == nil || !strings.Contains(err.Error(), "no non-loopback IPv4") {
		t.Fatalf("err = %v, want a clear no-address error", err)
	}
}

func TestContainerIPv4ListError(t *testing.T) {
	// Losing the interface list is not "no address": report what broke.
	boom := errors.New("boom")
	l := netLister{
		interfaces: func() ([]net.Interface, error) { return nil, boom },
		addrs:      func(net.Interface) ([]net.Addr, error) { return nil, nil },
	}
	if _, err := containerIPv4(l); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap boom", err)
	}
}
