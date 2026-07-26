package adopt

import (
	"strings"
	"testing"
)

func TestParseDialect(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Dialect
		wantErr bool
	}{
		{in: "classic", want: Classic},
		{in: "unifios", want: UniFiOS},
		{in: "CLASSIC", want: Classic},
		{in: " UniFiOS ", want: UniFiOS},
		{in: "", wantErr: true},
		{in: "unifi-os", wantErr: true},
		{in: "uos", wantErr: true},
	} {
		got, err := ParseDialect(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDialect(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDialect(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseDialect(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseDialectErrorNamesTheAccepted checks the error is actionable: a
// typo'd dialect must say what the accepted spellings are, because this
// value arrives from a container env var nobody can inspect interactively.
func TestParseDialectErrorNamesTheAccepted(t *testing.T) {
	_, err := ParseDialect("unifi-os")
	if err == nil {
		t.Fatal("ParseDialect with a bad value: want error, got nil")
	}
	for _, want := range []string{"unifi-os", "classic", "unifios"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDialectForURL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Dialect
	}{
		// UniFi OS fronts ucore on 443, explicit or as the https default.
		{in: "https://controller:443", want: UniFiOS},
		{in: "https://controller", want: UniFiOS},
		{in: "https://controller/", want: UniFiOS},
		// The classic Network App listens on 8443.
		{in: "https://controller:8443", want: Classic},
		{in: "http://controller:8080", want: Classic},
		// http with no port is 80, not 443.
		{in: "http://controller", want: Classic},
		// A nonstandard port is not UniFi OS by default; the operator
		// overrides with -adopt-dialect when the guess is wrong.
		{in: "https://controller:9443", want: Classic},
		// Unparseable input must not panic; Classic is the safe default
		// because the missing-CSRF failure mode is loud and immediate.
		{in: "://nonsense", want: Classic},
		{in: "", want: Classic},
	} {
		if got := DialectForURL(tc.in); got != tc.want {
			t.Errorf("DialectForURL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDialectString(t *testing.T) {
	if got := Classic.String(); got != "classic" {
		t.Errorf("Classic.String() = %q, want classic", got)
	}
	if got := UniFiOS.String(); got != "unifios" {
		t.Errorf("UniFiOS.String() = %q, want unifios", got)
	}
	if got := Dialect(42).String(); got != "unknown" {
		t.Errorf("Dialect(42).String() = %q, want unknown", got)
	}
}
