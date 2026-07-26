package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/internal/adopt"
)

// envMap turns a map into the getenv function the resolvers take, so a test
// names only the variables it cares about.
func envMap(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestAdoptEnvDefaultsEmptyEnvironment(t *testing.T) {
	got, err := adoptEnvDefaults(envMap(nil))
	if err != nil {
		t.Fatalf("adoptEnvDefaults: %v", err)
	}
	if got.enabled {
		t.Error("adoption is on with no SIM_ADOPT set; it must be opt-in")
	}
	if got.site != "default" {
		t.Errorf("site = %q, want default", got.site)
	}
	if got.timeout != 5*time.Minute {
		t.Errorf("timeout = %s, want 5m", got.timeout)
	}
}

func TestAdoptEnvDefaultsReadsEnvironment(t *testing.T) {
	got, err := adoptEnvDefaults(envMap(map[string]string{
		"SIM_ADOPT":         "true",
		"SIM_ADOPT_URL":     "https://controller:8443",
		"SIM_ADOPT_SITE":    "lab",
		"SIM_ADOPT_DIALECT": "classic",
		"SIM_ADOPT_TIMEOUT": "90s",
	}))
	if err != nil {
		t.Fatalf("adoptEnvDefaults: %v", err)
	}
	want := adoptSettings{
		enabled: true,
		url:     "https://controller:8443",
		site:    "lab",
		dialect: "classic",
		timeout: 90 * time.Second,
	}
	if got != want {
		t.Errorf("adoptEnvDefaults() = %+v, want %+v", got, want)
	}
}

func TestAdoptEnvDefaultsRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{
			name: "SIM_ADOPT is not a bool",
			vars: map[string]string{"SIM_ADOPT": "yes please"},
			want: "SIM_ADOPT",
		},
		{
			name: "SIM_ADOPT_TIMEOUT is not a duration",
			vars: map[string]string{"SIM_ADOPT_TIMEOUT": "five minutes"},
			want: "SIM_ADOPT_TIMEOUT",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := adoptEnvDefaults(envMap(tc.vars))
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

// enabledSettings is a complete, valid adopt configuration; individual tests
// break one field at a time.
func enabledSettings() adoptSettings {
	return adoptSettings{
		enabled: true,
		url:     "https://controller:8443",
		site:    "default",
		timeout: 5 * time.Minute,
	}
}

func failReadFile(string) ([]byte, error) {
	return nil, errors.New("readFile must not be called")
}

func TestResolveAdoptDisabled(t *testing.T) {
	got, err := resolveAdopt(adoptSettings{site: "default"}, envMap(nil), failReadFile)
	if err != nil {
		t.Fatalf("resolveAdopt: %v", err)
	}
	if got.enabled {
		t.Error("resolved config is enabled, want disabled")
	}
}

// TestResolveAdoptRejectsSettingsWithoutAdopt closes the silent-misconfig
// hole: credentials in the environment with SIM_ADOPT unset would otherwise
// boot a container that informs and never adopts, with nothing in the log.
func TestResolveAdoptRejectsSettingsWithoutAdopt(t *testing.T) {
	s := adoptSettings{site: "default", url: "https://controller:8443"}
	_, err := resolveAdopt(s, envMap(nil), failReadFile)
	if err == nil {
		t.Fatal("adopt settings with adoption off: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "-adopt") {
		t.Errorf("error %q does not say how to turn adoption on", err)
	}
}

// TestResolveAdoptAllowsExplicitOptOut is the other side of that guard.
// Turning adoption off with -adopt=false while the environment still
// carries SIM_ADOPT_* is how a compose override disables it, and it is not
// the forgotten-switch mistake the guard is looking for.
func TestResolveAdoptAllowsExplicitOptOut(t *testing.T) {
	s := adoptSettings{site: "default", url: "https://controller:8443", enabledExplicit: true}
	got, err := resolveAdopt(s, envMap(map[string]string{
		"SIM_ADOPT_USERNAME": "admin",
		"SIM_ADOPT_PASSWORD": "admin",
	}), failReadFile)
	if err != nil {
		t.Fatalf("explicit -adopt=false with settings present: %v", err)
	}
	if got.enabled {
		t.Error("resolved config is enabled, want disabled")
	}
}

func TestResolveAdoptReadsCredentials(t *testing.T) {
	got, err := resolveAdopt(enabledSettings(), envMap(map[string]string{
		"SIM_ADOPT_USERNAME": "admin",
		"SIM_ADOPT_PASSWORD": "hunter2",
	}), failReadFile)
	if err != nil {
		t.Fatalf("resolveAdopt: %v", err)
	}
	if got.username != "admin" || got.password != "hunter2" {
		t.Errorf("credentials = %q/%q, want admin/hunter2", got.username, got.password)
	}
}

// TestResolveAdoptPasswordFileWins covers the CI case: the workflow sets a
// mounted secret path and the file, not the inline value, is authoritative.
func TestResolveAdoptPasswordFileWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveAdopt(enabledSettings(), envMap(map[string]string{
		"SIM_ADOPT_USERNAME":      "admin",
		"SIM_ADOPT_PASSWORD":      "from-the-env",
		"SIM_ADOPT_PASSWORD_FILE": path,
	}), os.ReadFile)
	if err != nil {
		t.Fatalf("resolveAdopt: %v", err)
	}
	// The trailing newline every editor and `echo` adds is not part of the
	// password; leaving it in produces a login failure nobody can see.
	if got.password != "from-the-file" {
		t.Errorf("password = %q, want the file's contents without its newline", got.password)
	}
}

func TestResolveAdoptPasswordFileUnreadable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	_, err := resolveAdopt(enabledSettings(), envMap(map[string]string{
		"SIM_ADOPT_USERNAME":      "admin",
		"SIM_ADOPT_PASSWORD_FILE": missing,
	}), os.ReadFile)
	if err == nil {
		t.Fatal("unreadable password file: want an error, got nil")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the file", err)
	}
}

func TestResolveAdoptRequiresEverySetting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings adoptSettings
		vars     map[string]string
		want     string
	}{
		{
			name:     "no controller URL",
			settings: func() adoptSettings { s := enabledSettings(); s.url = ""; return s }(),
			vars:     map[string]string{"SIM_ADOPT_USERNAME": "admin", "SIM_ADOPT_PASSWORD": "admin"},
			want:     "-adopt-url",
		},
		{
			name:     "no username",
			settings: enabledSettings(),
			vars:     map[string]string{"SIM_ADOPT_PASSWORD": "admin"},
			want:     "SIM_ADOPT_USERNAME",
		},
		{
			name:     "no password",
			settings: enabledSettings(),
			vars:     map[string]string{"SIM_ADOPT_USERNAME": "admin"},
			want:     "SIM_ADOPT_PASSWORD",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveAdopt(tc.settings, envMap(tc.vars), os.ReadFile)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

// TestResolveAdoptRejectsNonPositiveTimeout catches a setting that would
// otherwise fail in the most confusing way available: a zero budget expires
// the moment adoption starts, so a perfectly healthy fleet gets reported as
// never adopting.
func TestResolveAdoptRejectsNonPositiveTimeout(t *testing.T) {
	creds := map[string]string{"SIM_ADOPT_USERNAME": "admin", "SIM_ADOPT_PASSWORD": "admin"}
	for _, timeout := range []time.Duration{0, -time.Second} {
		s := enabledSettings()
		s.timeout = timeout
		_, err := resolveAdopt(s, envMap(creds), os.ReadFile)
		if err == nil {
			t.Errorf("timeout %s: want an error, got nil", timeout)
			continue
		}
		if !strings.Contains(err.Error(), "-adopt-timeout") {
			t.Errorf("timeout %s: error %q does not name the setting", timeout, err)
		}
	}
}

func TestResolveAdoptDialect(t *testing.T) {
	creds := map[string]string{"SIM_ADOPT_USERNAME": "admin", "SIM_ADOPT_PASSWORD": "admin"}
	for _, tc := range []struct {
		name        string
		url         string
		dialect     string
		want        adopt.Dialect
		wantGuessed bool
	}{
		{name: "8443 guesses classic", url: "https://controller:8443", want: adopt.Classic, wantGuessed: true},
		{name: "443 guesses unifios", url: "https://controller:443", want: adopt.UniFiOS, wantGuessed: true},
		{name: "explicit beats the port", url: "https://controller:443", dialect: "classic", want: adopt.Classic},
		{name: "explicit on a stock port", url: "https://controller:8443", dialect: "unifios", want: adopt.UniFiOS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := enabledSettings()
			s.url, s.dialect = tc.url, tc.dialect
			got, err := resolveAdopt(s, envMap(creds), os.ReadFile)
			if err != nil {
				t.Fatalf("resolveAdopt: %v", err)
			}
			if got.dialect != tc.want {
				t.Errorf("dialect = %v, want %v", got.dialect, tc.want)
			}
			if got.dialectFromPort != tc.wantGuessed {
				t.Errorf("dialectFromPort = %v, want %v", got.dialectFromPort, tc.wantGuessed)
			}
		})
	}
}

// TestRunAdoptSkipsDiagnosisWhenInterrupted covers the shutdown path. A
// failed adoption normally dumps every device's controller state before the
// container exits, but when the failure *is* the interrupt there is nobody
// to read it and the extra round trips can push shutdown past the stop
// grace period, turning a clean SIGTERM into a SIGKILL.
func TestRunAdoptSkipsDiagnosisWhenInterrupted(t *testing.T) {
	parent, interrupt := context.WithCancel(context.Background())
	defer interrupt()

	var mu sync.Mutex
	statCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "s", Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
	})
	mux.HandleFunc("GET /api/s/{site}/stat/device", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		statCalls++
		first := statCalls == 1
		mu.Unlock()
		if first {
			interrupt() // the signal lands mid-adoption
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{"rc":"ok"},"data":[{"mac":"00:15:6d:00:00:01","state":2,"adopted":false}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := adoptConfig{
		enabled: true, url: srv.URL, site: "default",
		dialect: adopt.Classic, username: "admin", password: "admin",
		timeout: time.Minute,
	}
	macs := []string{"00:15:6d:00:00:01", "00:15:6d:00:00:02", "00:15:6d:00:00:03"}
	if err := runAdopt(parent, cfg, macs); err == nil {
		t.Fatal("runAdopt through an interrupt: want an error, got nil")
	}

	mu.Lock()
	defer mu.Unlock()
	// One read found the device pending; the adopt then failed on the
	// cancelled context. A diagnosis would add one read per MAC.
	if statCalls != 1 {
		t.Errorf("controller saw %d stat/device reads, want 1 — the post-mortem ran during shutdown", statCalls)
	}
}

func TestResolveAdoptRejectsUnknownDialect(t *testing.T) {
	s := enabledSettings()
	s.dialect = "unifi-os"
	_, err := resolveAdopt(s, envMap(map[string]string{
		"SIM_ADOPT_USERNAME": "admin",
		"SIM_ADOPT_PASSWORD": "admin",
	}), os.ReadFile)
	if err == nil {
		t.Fatal("unknown dialect: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "unifi-os") {
		t.Errorf("error %q does not name the bad value", err)
	}
}
