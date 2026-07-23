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

// fakeClassic is an in-memory classic Network App controller: cookie login,
// devmgr adopt, and a stat/device list that flips the configured device to
// state 1 / adopted once an adopt command for its MAC has been seen.
type fakeClassic struct {
	server *httptest.Server
	mac    string

	mu       sync.Mutex
	adopted  bool
	sawAdopt []string
}

func newFakeClassic(t *testing.T, mac string) *fakeClassic {
	t.Helper()
	f := &fakeClassic{mac: mac}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", f.handleLogin)
	mux.HandleFunc("POST /api/s/{site}/cmd/devmgr", f.handleDevmgr)
	mux.HandleFunc("GET /api/s/{site}/stat/device", f.handleStatDevice)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// writeOK mirrors the real controller's success envelope.
func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
}

// writeErr mirrors the real controller's error envelope: the msg field
// carries the api.err.* reason the client needs for live debugging.
func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"meta": map[string]any{"rc": "error", "msg": msg},
	})
}

func (f *fakeClassic) handleLogin(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if creds.Username != "admin" || creds.Password != "admin" {
		writeErr(w, http.StatusUnauthorized, "api.err.InvalidCredential")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "test-session", Path: "/"})
	writeOK(w)
}

func (f *fakeClassic) handleDevmgr(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie("unifises"); err != nil {
		writeErr(w, http.StatusUnauthorized, "api.err.Unauthorized")
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
	if cmd.MAC == f.mac {
		f.adopted = true
	}
	f.mu.Unlock()
	writeOK(w)
}

func (f *fakeClassic) handleStatDevice(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie("unifises"); err != nil {
		writeErr(w, http.StatusUnauthorized, "api.err.Unauthorized")
		return
	}
	f.mu.Lock()
	state, adopted := 2, false
	if f.adopted {
		state, adopted = 1, true
	}
	f.mu.Unlock()
	// Real stat/device docs carry dozens more fields; Device must ignore them.
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

func (f *fakeClassic) sawAdopts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sawAdopt...)
}

func TestClassicAdoptFlow(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	c := NewClassicClient(f.server.URL)
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
}

func TestClassicLoginFailure(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	c := NewClassicClient(f.server.URL)
	ctx := waitCtx(t, 5*time.Second)

	// The controller puts the real reason in the body's msg field; the
	// client error must surface it for live debugging.
	err := c.Login(ctx, "admin", "wrong")
	if err == nil {
		t.Fatal("Login with bad password: want error, got nil")
	}
	if !strings.Contains(err.Error(), "api.err.InvalidCredential") {
		t.Errorf("Login error %q does not carry the body's msg", err)
	}
	// Without a session cookie the controller rejects API calls too.
	if err := c.Adopt(ctx, "default", mac); err == nil ||
		!strings.Contains(err.Error(), "api.err.Unauthorized") {
		t.Errorf("Adopt without login = %v, want api.err.Unauthorized", err)
	}
	if _, err := c.DeviceByMAC(ctx, "default", mac); err == nil ||
		!strings.Contains(err.Error(), "api.err.Unauthorized") {
		t.Errorf("DeviceByMAC without login = %v, want api.err.Unauthorized", err)
	}
}

func TestClassicDeviceByMACNotFound(t *testing.T) {
	f := newFakeClassic(t, "00:15:6d:00:00:01")
	c := NewClassicClient(f.server.URL)
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

func TestClassicWaitAdoptedTimeout(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac) // nobody ever clicks Adopt
	c := NewClassicClient(f.server.URL)
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

func TestClassicRejectsHTTP200ErrorEnvelopes(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusOK, "api.err.InvalidTarget")
	}))
	t.Cleanup(s.Close)
	c := NewClassicClient(s.URL)
	ctx := waitCtx(t, 5*time.Second)

	if err := c.Login(ctx, "admin", "admin"); err == nil ||
		!strings.Contains(err.Error(), "api.err.InvalidTarget") {
		t.Errorf("Login 200 error envelope = %v, want InvalidTarget", err)
	}
	if err := c.Adopt(ctx, "default", "00:15:6d:00:00:01"); err == nil ||
		!strings.Contains(err.Error(), "api.err.InvalidTarget") {
		t.Errorf("Adopt 200 error envelope = %v, want InvalidTarget", err)
	}
	if _, err := c.DeviceByMAC(ctx, "default", "00:15:6d:00:00:01"); err == nil ||
		!strings.Contains(err.Error(), "api.err.InvalidTarget") {
		t.Errorf("DeviceByMAC 200 error envelope = %v, want InvalidTarget", err)
	}
}
