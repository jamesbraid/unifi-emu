package main

import "testing"

// resolveInformURL rewrites hostname inform URLs to the resolved IPv4 so
// the reported inform_url passes controller-side validation; IPs and
// unresolvable hosts pass through unchanged.
func TestResolveInformURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ipv4 passthrough", "http://192.168.1.10:8080/inform", "http://192.168.1.10:8080/inform"},
		{"ipv4 no port", "http://192.168.1.10/inform", "http://192.168.1.10/inform"},
		{"ipv6 passthrough", "http://[fd00::1]:8080/inform", "http://[fd00::1]:8080/inform"},
		{"hostname resolved", "http://localhost:8080/inform", "http://127.0.0.1:8080/inform"},
		{"hostname no port", "http://localhost/inform", "http://127.0.0.1/inform"},
		{"unresolvable kept", "http://no.such.host.invalid:8080/inform", "http://no.such.host.invalid:8080/inform"},
		{"not a url kept", "garbage", "garbage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveInformURL(tt.in); got != tt.want {
				t.Errorf("resolveInformURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
