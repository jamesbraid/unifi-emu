package adopt

import (
	"fmt"
	"net/url"
	"strings"
)

// Dialect selects which controller front end the client talks to: the
// classic Network App API (:8443, cookie auth, /api/s/<site>/…) or the newer
// UniFi OS proxy (:443, /api/auth/login + rotating CSRF, paths under
// /proxy/network). The wire flow is otherwise identical, so one client
// covers both.
type Dialect int

const (
	Classic Dialect = iota
	UniFiOS
)

func (d Dialect) String() string {
	switch d {
	case Classic:
		return "classic"
	case UniFiOS:
		return "unifios"
	}
	return "unknown"
}

// ParseDialect reads the spelling used by -adopt-dialect and
// SIM_ADOPT_DIALECT. The error names both accepted values because this
// setting usually arrives as a container env var, where a typo produces no
// other diagnostic.
func ParseDialect(s string) (Dialect, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "classic":
		return Classic, nil
	case "unifios":
		return UniFiOS, nil
	}
	return Classic, fmt.Errorf("emu: unknown adopt dialect %q; want classic or unifios", s)
}

// DialectForURL guesses the dialect from the controller API URL's port:
// UniFi OS fronts ucore on 443 (explicit, or implied by an https URL with no
// port), and everything else — 8443 above all — is the classic Network App.
//
// This is a heuristic, not a protocol fact, which is why the caller logs
// what it resolved to and -adopt-dialect overrides it. Classic is the safe
// default for anything unparseable: guessing UniFiOS wrongly fails at login
// with a missing-CSRF error that reads like a controller bug.
func DialectForURL(rawURL string) Dialect {
	u, err := url.Parse(rawURL)
	if err != nil {
		return Classic
	}
	port := u.Port()
	if port == "" && strings.EqualFold(u.Scheme, "https") {
		port = "443"
	}
	if port == "443" {
		return UniFiOS
	}
	return Classic
}
