package emu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Token values the fake issues: ucore hands out one at login, then rotates
// it mid-session and returns the replacement on an ordinary response.
const (
	uosTokenV1 = "csrf-test-token"
	uosTokenV2 = "csrf-rotated-token"
)

// fakeUOS is an in-memory UniFi OS controller front end: /api/auth/login
// hands out a session cookie plus a CSRF token in the
// x-updated-csrf-token response header, and the /proxy/network API 403s any
// call that lacks either — the way ucore guards the proxied Network App.
// The adopt command rotates the token (the response carries the
// replacement), mirroring ucore's mid-session rotation. stat/device flips
// the configured device to state 1 / adopted once an adopt command for its
// MAC has been seen.
type fakeUOS struct {
	server   *httptest.Server
	mac      string
	omitCSRF bool // simulate a controller that never sends the token header

	mu       sync.Mutex
	csrf     string // currently valid token; rotated by the adopt command
	adopted  bool
	sawAdopt []string
	sawCSRF  []string // X-CSRF-Token value on every authed-endpoint hit
}

func newFakeUOS(t *testing.T, mac string) *fakeUOS {
	t.Helper()
	f := &fakeUOS{mac: mac, csrf: uosTokenV1}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", f.handleLogin)
	mux.HandleFunc("POST /proxy/network/api/s/{site}/cmd/devmgr", f.handleDevmgr)
	mux.HandleFunc("GET /proxy/network/api/s/{site}/stat/device", f.handleStatDevice)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeUOS) handleLogin(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if creds.Username != "admin" || creds.Password != "admin" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    "AUTHENTICATION_FAILED",
			"message": "invalid username or password",
		})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "uos_session", Value: "test-session", Path: "/"})
	if !f.omitCSRF {
		f.mu.Lock()
		tok := f.csrf
		f.mu.Unlock()
		w.Header().Set("x-updated-csrf-token", tok)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"uos-user"}`))
}

// authed reports whether r carries the session cookie and the currently
// valid CSRF token; ucore 403s anything else under /proxy/network.
func (f *fakeUOS) authed(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	f.sawCSRF = append(f.sawCSRF, r.Header.Get("X-CSRF-Token"))
	valid := f.csrf
	f.mu.Unlock()
	if _, err := r.Cookie("uos_session"); err != nil {
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
		return false
	}
	if r.Header.Get("X-CSRF-Token") != valid {
		http.Error(w, `{"message":"invalid csrf token"}`, http.StatusForbidden)
		return false
	}
	return true
}

func (f *fakeUOS) handleDevmgr(w http.ResponseWriter, r *http.Request) {
	if !f.authed(w, r) {
		return
	}
	var cmd struct {
		Cmd string `json:"cmd"`
		MAC string `json:"mac"`
	}
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if cmd.Cmd != "adopt" {
		http.Error(w, "unsupported cmd", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.sawAdopt = append(f.sawAdopt, cmd.MAC)
	rotated := ""
	if cmd.MAC == f.mac {
		f.adopted = true
		// ucore rotates the token mid-session and returns the
		// replacement in a response header; the old token 403s from
		// here on.
		f.csrf = uosTokenV2
		rotated = uosTokenV2
	}
	f.mu.Unlock()
	if rotated != "" {
		w.Header().Set("x-updated-csrf-token", rotated)
	}
	writeOK(w)
}

func (f *fakeUOS) handleStatDevice(w http.ResponseWriter, r *http.Request) {
	if !f.authed(w, r) {
		return
	}
	f.mu.Lock()
	state, adopted := 2, false
	if f.adopted {
		state, adopted = 1, true
	}
	f.mu.Unlock()
	doc := map[string]any{
		"data": []map[string]any{{
			"mac":     f.mac,
			"state":   state,
			"adopted": adopted,
			"model":   "U7MP",
			"ip":      "10.0.0.57",
			"name":    "emu",
			"uptime":  12345,
			"version": "7.6.42",
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (f *fakeUOS) sawAdopts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sawAdopt...)
}

func (f *fakeUOS) sawTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sawCSRF...)
}

func TestUOSAdoptFlow(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeUOS(t, mac)
	c := NewUOSClient(f.server.URL)
	ctx := waitCtx(t, 5*time.Second)

	if err := c.Login(ctx, "admin", "admin"); err != nil {
		t.Fatalf("Login: %v", err)
	}

	// The device sits pending until the fake sees the adopt command.
	d, err := c.DeviceByMAC(ctx, "default", mac)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	if d.State != 2 || d.Adopted {
		t.Errorf("pre-adopt device = %+v, want state 2 not adopted", d)
	}

	if err := c.Adopt(ctx, "default", mac); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	d, err = c.WaitAdopted(ctx, "default", mac)
	if err != nil {
		t.Fatalf("WaitAdopted: %v", err)
	}
	if d.State != 1 || !d.Adopted {
		t.Errorf("WaitAdopted device = %+v, want state 1 adopted", d)
	}
	if saw := f.sawAdopts(); len(saw) != 1 || saw[0] != mac {
		t.Errorf("fake saw adopt MACs %v, want [%s]", saw, mac)
	}
	// Every authed call must have carried a token the fake had issued —
	// anything else would have been a 403 from ucore — and the calls after
	// the adopt must carry the rotated token: the client had to follow the
	// rotation, or WaitAdopted above would have 403d until it timed out.
	tokens := f.sawTokens()
	if len(tokens) == 0 {
		t.Fatal("fake saw no authed calls")
	}
	for _, tok := range tokens {
		if tok != uosTokenV1 && tok != uosTokenV2 {
			t.Errorf("authed call carried X-CSRF-Token %q, want an issued token", tok)
		}
	}
	if last := tokens[len(tokens)-1]; last != uosTokenV2 {
		t.Errorf("last authed call carried X-CSRF-Token %q, want rotated %q", last, uosTokenV2)
	}
}

func TestUOSLoginFailure(t *testing.T) {
	f := newFakeUOS(t, "00:15:6d:00:00:01")
	c := NewUOSClient(f.server.URL)
	ctx := waitCtx(t, 5*time.Second)

	// ucore puts the real reason in the body; the client error must
	// surface it for live debugging.
	err := c.Login(ctx, "admin", "wrong")
	if err == nil {
		t.Fatal("Login with bad password: want error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid username or password") {
		t.Errorf("Login error %q does not carry the body's message", err)
	}
}

func TestUOSLoginMissingToken(t *testing.T) {
	f := newFakeUOS(t, "00:15:6d:00:00:01")
	f.omitCSRF = true // 200 but no x-updated-csrf-token header
	c := NewUOSClient(f.server.URL)

	err := c.Login(waitCtx(t, 5*time.Second), "admin", "admin")
	if err == nil {
		t.Fatal("Login with no CSRF token header: want error, got nil")
	}
	if !strings.Contains(err.Error(), "x-updated-csrf-token") {
		t.Errorf("Login error %q does not name the missing header", err)
	}
}

func TestUOSAuthedCallsNeedLogin(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeUOS(t, mac)
	c := NewUOSClient(f.server.URL) // no Login: no cookie, no CSRF token
	ctx := waitCtx(t, 5*time.Second)

	// ucore 403s proxied calls without cookie + token, so both must fail.
	if err := c.Adopt(ctx, "default", mac); err == nil ||
		!strings.Contains(err.Error(), "403") {
		t.Errorf("Adopt without login = %v, want HTTP 403", err)
	}
	if _, err := c.DeviceByMAC(ctx, "default", mac); err == nil ||
		!strings.Contains(err.Error(), "403") {
		t.Errorf("DeviceByMAC without login = %v, want HTTP 403", err)
	}
}

func TestUOSDeviceByMACNotFound(t *testing.T) {
	f := newFakeUOS(t, "00:15:6d:00:00:01")
	c := NewUOSClient(f.server.URL)
	ctx := waitCtx(t, 5*time.Second)
	if err := c.Login(ctx, "admin", "admin"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	_, err := c.DeviceByMAC(ctx, "default", "00:15:6d:00:00:99")
	if err == nil {
		t.Fatal("DeviceByMAC unknown MAC: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not say not found", err)
	}
}

func TestUOSWaitAdoptedTimeout(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeUOS(t, mac) // nobody ever clicks Adopt
	c := NewUOSClient(f.server.URL)
	if err := c.Login(context.Background(), "admin", "admin"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	d, err := c.WaitAdopted(waitCtx(t, 300*time.Millisecond), "default", mac)
	if err == nil {
		t.Fatal("WaitAdopted with no adoption: want error, got nil")
	}
	if d.State != 2 || d.Adopted {
		t.Errorf("last seen device = %+v, want pending state 2", d)
	}
	if !strings.Contains(err.Error(), "State:2") {
		t.Errorf("timeout error %q does not include the last seen device state", err)
	}
}
