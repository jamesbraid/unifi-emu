package herder

import (
	"net"
	"net/url"
	"strconv"
)

// CheckInformURL validates the caller-supplied inform endpoint. Its exact
// form is http://<canonical-IPv4-literal>:<port>/inform.
//
// The narrowness is the contract, not fussiness: device containers resolve
// nothing and the controller rejects an inform whose host is not an IP it
// recognizes, so a hostname, an IPv6 literal or a loopback address produces a
// fleet that starts cleanly and never adopts. Failing here names the problem
// while it is still one line of caller configuration.
func CheckInformURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return wrapf(err, CodeInformURLInvalid, PhaseValidate, "parse inform URL %q: %v", raw, err)
	}
	if u.Scheme != "http" {
		return failf(CodeInformURLInvalid, PhaseValidate,
			"inform URL %q must use http, got %q", raw, u.Scheme)
	}
	if u.User != nil {
		return failf(CodeInformURLInvalid, PhaseValidate, "inform URL %q carries user information", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return failf(CodeInformURLInvalid, PhaseValidate, "inform URL %q carries a query or fragment", raw)
	}
	if u.Path != "/inform" {
		return failf(CodeInformURLInvalid, PhaseValidate,
			"inform URL %q path is %q, want /inform", raw, u.Path)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return wrapf(err, CodeInformURLInvalid, PhaseValidate,
			"inform URL %q needs an explicit port: %v", raw, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		return failf(CodeInformURLInvalid, PhaseValidate,
			"inform URL host %q is not an IPv4 literal", host)
	}
	// A canonical literal round-trips. Anything else -- a leading-zero
	// octet, an IPv4-mapped IPv6 spelling -- would reach the controller as
	// a different string than the one it compares against.
	if ip.To4().String() != host {
		return failf(CodeInformURLInvalid, PhaseValidate,
			"inform URL host %q is not canonical IPv4 text", host)
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return failf(CodeInformURLInvalid, PhaseValidate,
			"inform URL host %q is not reachable from a device container", host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return failf(CodeInformURLInvalid, PhaseValidate, "inform URL port %q is not 1-65535", port)
	}
	return nil
}
