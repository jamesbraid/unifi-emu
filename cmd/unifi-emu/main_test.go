package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// resolveInformURL rewrites hostname inform URLs to the resolved IPv4 so
// the reported inform_url passes controller-side validation; IPs and
// invalid or unresolvable hostnames fail before the emulator starts.
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveInformURL(tt.in)
			if err != nil {
				t.Fatalf("resolveInformURL(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("resolveInformURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveInformURLRejectsInvalidOrUnresolvableHosts(t *testing.T) {
	lookupErr := errors.New("dns unavailable")
	tests := []struct {
		name   string
		raw    string
		lookup func(context.Context, string, string) ([]net.IP, error)
		want   string
	}{
		{"not a URL", "garbage", nil, "valid inform URL"},
		{"missing host", "http:///inform", nil, "host"},
		{"lookup error", "http://controller:8080/inform", func(context.Context, string, string) ([]net.IP, error) {
			return nil, lookupErr
		}, "dns unavailable"},
		{"no IPv4", "http://controller:8080/inform", func(context.Context, string, string) ([]net.IP, error) {
			return nil, nil
		}, "no IPv4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := tt.lookup
			if lookup == nil {
				lookup = func(context.Context, string, string) ([]net.IP, error) {
					t.Fatal("lookup called for malformed URL")
					return nil, nil
				}
			}
			_, err := resolveInformURLWith(context.Background(), tt.raw, lookup)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}
