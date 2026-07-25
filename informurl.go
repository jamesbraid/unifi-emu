package emu

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
)

// ResolveInformURL rewrites raw's host to its resolved IPv4 address so the
// inform_url a device reports survives controller-side validation. Recent
// controllers reject an inform whose host is not an IP they recognize
// ("invalid inform_ip <host>", HTTP 400 once adoption starts), which
// deadlocks the device in ADOPTING when the fleet is pointed at a DNS name
// such as http://unifi:8080/inform. Dialing the resolved IP is equivalent and
// reports an inform_url the controller accepts.
//
// IP literals (v4 or v6) pass through unchanged. A malformed URL, a missing
// host, or a hostname with no IPv4 address is an error, so a caller can fail
// fast before starting the fleet rather than stall at adoption. New keeps its
// informURL verbatim, so a caller handing it a hostname should resolve here
// first; the CLI does exactly this at startup. The caller owns the timeout via
// ctx.
func ResolveInformURL(ctx context.Context, raw string) (string, error) {
	return resolveInformURLWith(ctx, raw, net.DefaultResolver.LookupIP)
}

func resolveInformURLWith(
	ctx context.Context,
	raw string,
	lookup func(context.Context, string, string) ([]net.IP, error),
) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		if err == nil {
			err = errors.New("missing scheme or host")
		}
		return "", fmt.Errorf("not a valid inform URL %q: %w", raw, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("inform URL %q has no host", raw)
	}
	if net.ParseIP(host) != nil {
		return raw, nil
	}
	ips, err := lookup(ctx, "ip4", host)
	if err != nil {
		return "", fmt.Errorf("resolve inform host %q to IPv4: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("inform host %q has no IPv4 address", host)
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(ips[0].String(), port)
	} else {
		u.Host = ips[0].String()
	}
	return u.String(), nil
}
