package unifiemu

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

func writeRC(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"meta":{"rc":"ok"}}`))
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
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "test-session", Path: "/"})
	writeRC(w)
}

func (f *fakeClassic) handleDevmgr(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie("unifises"); err != nil {
		http.Error(w, "no session", http.StatusUnauthorized)
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
	writeRC(w)
}

func (f *fakeClassic) handleStatDevice(w http.ResponseWriter, r *http.Request) {
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
	f := newFakeClassic(t, "00:15:6d:00:00:01")
	c := NewClassicClient(f.server.URL)
	ctx := waitCtx(t, 5*time.Second)

	if err := c.Login(ctx, "admin", "wrong"); err == nil {
		t.Fatal("Login with bad password: want error, got nil")
	}
	// Without a session cookie the controller rejects the adopt call too.
	if err := c.Adopt(ctx, "default", "00:15:6d:00:00:01"); err == nil {
		t.Error("Adopt without login: want error, got nil")
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
