package adopt

// Unit coverage for the Client against in-memory fakes: both dialects'
// login, CSRF capture and mid-session rotation (UniFiOS), adopt carrying the
// CSRF header on UniFiOS and none on Classic, DeviceByMAC found/not-found,
// the meta.rc!=ok envelope check, and WaitAdopted settling on state 1.

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

// waitCtx returns a context that cancels after d and is cleaned up with the
// test.
func waitCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// writeOK mirrors the real controller's success envelope.
func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
}

// writePlaceholder mirrors the HTML page a controller serves under HTTP 200
// on every path until it has finished starting.
func writePlaceholder(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte("<html><body>UniFi is starting</body></html>"))
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

// deviceDoc is a stat/device response carrying dozens of fields the client
// must ignore; state/adopted reflect whether the fake has seen the adopt.
func deviceDoc(mac string, state int, adopted bool) map[string]any {
	return map[string]any{"data": []map[string]any{deviceEntry(mac, state, adopted)}}
}

// deviceEntry is one device inside a stat/device list.
func deviceEntry(mac string, state int, adopted bool) map[string]any {
	return map[string]any{
		"mac":     mac,
		"state":   state,
		"adopted": adopted,
		"model":   "U7MP",
		"ip":      "10.0.0.57",
		"name":    "emu",
		"uptime":  12345,
		"version": "7.6.42",
	}
}

// fakeClassic is an in-memory classic Network App controller: cookie login,
// devmgr adopt, and a stat/device list that flips a configured device to
// state 1 / adopted once an adopt command for its MAC has been seen. It
// lists every MAC it was built with, so a fleet test can drive several.
type fakeClassic struct {
	server *httptest.Server
	macs   []string

	// adoptErr, when non-empty, is the meta.msg returned for the first
	// failN adopt commands, so a test can drive the retry path.
	adoptErr string
	failN    int

	// placeholderAdopts is how many adopt commands are answered with the
	// boot placeholder instead of a reply. On ucore the login lands against
	// the OS while the Network App behind /proxy/network is still starting,
	// so an authenticated call can meet the placeholder long after login
	// succeeded.
	placeholderAdopts int

	mu       sync.Mutex
	adopted  map[string]bool
	sawAdopt []string
	sawCSRF  []string // X-CSRF-Token value on every authed hit (classic sends none)
	// forceState, when non-zero, replaces the pending/adopted flip, so a
	// test can present a controller verdict such as 7 (adopt-failed).
	forceState int
}

// setState makes the fake report state unconditionally.
func (f *fakeClassic) setState(state int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceState = state
}

func newFakeClassic(t *testing.T, macs ...string) *fakeClassic {
	t.Helper()
	f := &fakeClassic{macs: macs, adopted: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", f.handleLogin)
	mux.HandleFunc("POST /api/s/{site}/cmd/devmgr", f.handleDevmgr)
	mux.HandleFunc("GET /api/s/{site}/stat/device", f.handleStatDevice)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// markAdopted pre-adopts mac, so a test can present a device that is already
// connected before anything calls Adopt.
func (f *fakeClassic) markAdopted(mac string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adopted[mac] = true
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
	f.mu.Lock()
	f.sawCSRF = append(f.sawCSRF, r.Header.Get("X-CSRF-Token"))
	f.mu.Unlock()
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
	if len(f.sawAdopt) <= f.placeholderAdopts {
		f.mu.Unlock()
		writePlaceholder(w)
		return
	}
	reject := f.adoptErr != "" && len(f.sawAdopt) <= f.failN
	if !reject {
		for _, mac := range f.macs {
			if mac == cmd.MAC {
				f.adopted[mac] = true
			}
		}
	}
	adoptErr := f.adoptErr
	f.mu.Unlock()
	if reject {
		writeErr(w, http.StatusBadRequest, adoptErr)
		return
	}
	writeOK(w)
}

func (f *fakeClassic) handleStatDevice(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie("unifises"); err != nil {
		writeErr(w, http.StatusUnauthorized, "api.err.Unauthorized")
		return
	}
	f.mu.Lock()
	entries := make([]map[string]any, 0, len(f.macs))
	for _, mac := range f.macs {
		state, adopted := 2, false
		if f.adopted[mac] {
			state, adopted = 1, true
		}
		if f.forceState != 0 {
			state, adopted = f.forceState, false
		}
		entries = append(entries, deviceEntry(mac, state, adopted))
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
}

func (f *fakeClassic) sawAdopts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sawAdopt...)
}

func (f *fakeClassic) sawTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sawCSRF...)
}

// Token values fakeUOS issues: ucore hands out one at login, then rotates it
// mid-session and returns the replacement on an ordinary response.
const (
	uosTokenV1 = "csrf-test-token"
	uosTokenV2 = "csrf-rotated-token"
)

// fakeUOS is an in-memory UniFi OS controller front end: /api/auth/login
// hands out a session cookie plus a CSRF token in the x-updated-csrf-token
// response header, and the /proxy/network API 403s any call that lacks
// either — the way ucore guards the proxied Network App. The adopt command
// rotates the token (the response carries the replacement), mirroring
// ucore's mid-session rotation. stat/device flips the configured device to
// state 1 / adopted once an adopt command for its MAC has been seen.
type fakeUOS struct {
	server     *httptest.Server
	mac        string
	omitCSRF   bool // simulate a controller that never sends the token header
	legacyCSRF bool // send X-Csrf-Token instead of X-Updated-Csrf-Token

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
		header := "x-updated-csrf-token"
		if f.legacyCSRF {
			header = "x-csrf-token"
		}
		w.Header().Set(header, tok)
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
		// ucore rotates the token mid-session and returns the replacement
		// in a response header; the old token 403s from here on.
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(deviceDoc(f.mac, state, adopted))
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

// State 7 is widely documented as "adopt-failed", but a healthy classic
// adoption was measured going 2 -> 7 -> 5 -> 1 with adopted=true from 7
// onward: it is a step in the adopt-key changeover, not a verdict. WaitAdopted
// must therefore poll straight through it. Bailing out on 7 failed
// TestClassicUGWLive two seconds into an adoption that was working.
func TestClassicWaitAdoptedPollsThroughState7(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	f.setState(7)
	c := New(f.server.URL, Classic)
	ctx := waitCtx(t, 5*time.Second)

	if err := c.Login(ctx, "admin", "admin"); err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Waiting out the deadline is the pass condition: it proves the poll
	// treated 7 as "not there yet" rather than as a failure to report.
	d, err := c.WaitAdopted(waitCtx(t, 300*time.Millisecond), "default", mac)
	if err == nil {
		t.Fatalf("WaitAdopted never reached state 1: want the deadline error, got %+v", d)
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("WaitAdopted error = %v, want a deadline error rather than a state-7 verdict", err)
	}
	if d.State != 7 {
		t.Errorf("WaitAdopted last device = %+v, want the state-7 doc it kept polling past", d)
	}
}

// Devices reports the whole list, which is what a device that never appears
// needs: "not found" alone cannot say whether the controller dropped one
// device or listed nothing at all.
func TestClassicDevicesListsEveryDocument(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	c := New(f.server.URL, Classic)
	ctx := waitCtx(t, 5*time.Second)

	if err := c.Login(ctx, "admin", "admin"); err != nil {
		t.Fatalf("Login: %v", err)
	}

	list, err := c.Devices(ctx, "default")
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("Devices returned %d docs, want 1: %+v", len(list), list)
	}
	if list[0].MAC != mac || list[0].State != 2 {
		t.Errorf("Devices[0] = %+v, want %s pending", list[0], mac)
	}
}

func TestClassicAdoptFlow(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	c := New(f.server.URL, Classic)
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
	// The classic dialect authenticates by cookie alone: no call carries a
	// CSRF header.
	for _, tok := range f.sawTokens() {
		if tok != "" {
			t.Errorf("classic authed call carried X-CSRF-Token %q, want none", tok)
		}
	}
}

func TestClassicLoginFailure(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	c := New(f.server.URL, Classic)
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
	c := New(f.server.URL, Classic)
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
	c := New(f.server.URL, Classic)
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
	c := New(s.URL, Classic)
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

func TestUOSAdoptFlow(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeUOS(t, mac)
	c := New(f.server.URL, UniFiOS)
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

// TestLoginRetriesUntilTheContextExpires pins the behaviour a container
// service depends on: started before its controller, it must keep trying for
// as long as the adopt budget allows. A fixed attempt ceiling would give up
// while the caller still had minutes left and report a controller that had
// merely not finished booting as a failed adoption.
func TestLoginRetriesUntilTheContextExpires(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		http.Error(w, "still starting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	// Backoff in milliseconds, so a fixed ceiling would be exhausted long
	// before the deadline and the two causes stay distinguishable.
	c := newClient(srv.URL, Classic, time.Millisecond, 2*time.Millisecond)
	const budget = 300 * time.Millisecond
	start := time.Now()
	err := c.Login(waitCtx(t, budget), "admin", "admin")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Login against a controller that never comes up: want an error, got nil")
	}
	if elapsed < budget/2 {
		t.Errorf("Login gave up after %s of a %s budget; the context should be what stops it", elapsed, budget)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts < 10 {
		t.Errorf("controller saw %d attempts in %s; retrying stopped early", attempts, elapsed)
	}
}

// TestLoginDoesNotRetryRejectedCredentials is the other half: a password the
// controller refuses will be refused again, so retrying it would burn the
// whole budget before reporting a fault visible on the first attempt.
func TestLoginDoesNotRetryRejectedCredentials(t *testing.T) {
	f := newFakeClassic(t, "00:15:6d:00:00:01")
	c := newClient(f.server.URL, Classic, time.Millisecond, 2*time.Millisecond)

	start := time.Now()
	err := c.Login(waitCtx(t, 5*time.Second), "admin", "wrong")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Login with a bad password: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "api.err.InvalidCredential") {
		t.Errorf("error %q lost the controller's reason to a retry loop", err)
	}
	if elapsed > time.Second {
		t.Errorf("Login took %s to reject a bad password; it should fail on the first answer", elapsed)
	}
}

// bootingLogin answers the first n requests with the HTML placeholder a
// controller serves under HTTP 200 on every path while it is still starting,
// then hands over to ready. It also reports how many requests it saw, so a
// test can prove the retrying actually happened.
func bootingLogin(n int, ready http.HandlerFunc) (http.HandlerFunc, func() int) {
	var mu sync.Mutex
	seen := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++
		booting := seen <= n
		mu.Unlock()
		if booting {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>UniFi is starting</body></html>"))
			return
		}
		ready(w, r)
	}
	return handler, func() int {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

// TestClassicLoginRetriesThroughTheBootPlaceholder covers the cold start no
// status-code rule can catch: a controller that has not finished booting
// serves an HTML page under HTTP 200, so the retrying go-retryablehttp does
// for 429/5xx never fires and login fails on the first answer with a JSON
// decode error. That reports a controller which was merely still starting as
// a failed adoption, and kills a container started alongside its controller.
func TestClassicLoginRetriesThroughTheBootPlaceholder(t *testing.T) {
	handler, seen := bootingLogin(3, func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "test-session", Path: "/"})
		writeOK(w)
	})
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, Classic, time.Millisecond, 2*time.Millisecond)
	if err := c.Login(waitCtx(t, 5*time.Second), "admin", "admin"); err != nil {
		t.Fatalf("Login through a booting controller: %v", err)
	}
	if got := seen(); got != 4 {
		t.Errorf("controller saw %d login attempts, want 4 (3 placeholders then the real one)", got)
	}
}

// TestUOSLoginRetriesThroughTheBootPlaceholder is the same cold start on
// ucore, where the placeholder fails differently — 200 with no CSRF header —
// but just as fatally.
func TestUOSLoginRetriesThroughTheBootPlaceholder(t *testing.T) {
	handler, seen := bootingLogin(3, func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "uos_session", Value: "test-session", Path: "/"})
		w.Header().Set("x-updated-csrf-token", uosTokenV1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"uos-user"}`))
	})
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, UniFiOS, time.Millisecond, 2*time.Millisecond)
	if err := c.Login(waitCtx(t, 5*time.Second), "admin", "admin"); err != nil {
		t.Fatalf("Login through a booting controller: %v", err)
	}
	if got := seen(); got != 4 {
		t.Errorf("controller saw %d login attempts, want 4 (3 placeholders then the real one)", got)
	}
}

// TestLoginReportsEveryWait keeps a cold start from being a silent one. The
// wait can run for minutes, and an operator watching a container that has
// said nothing cannot tell a booting controller from a hung one.
func TestLoginReportsEveryWait(t *testing.T) {
	handler, _ := bootingLogin(2, func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "test-session", Path: "/"})
		writeOK(w)
	})
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, Classic, time.Millisecond, 2*time.Millisecond)
	var waits []string
	c.OnWaiting = func(detail string) { waits = append(waits, detail) }

	if err := c.Login(waitCtx(t, 5*time.Second), "admin", "admin"); err != nil {
		t.Fatalf("Login through a booting controller: %v", err)
	}
	if len(waits) != 2 {
		t.Fatalf("OnWaiting fired %d times, want 2 (one per placeholder): %q", len(waits), waits)
	}
	if !strings.Contains(waits[0], "200") || !strings.Contains(waits[0], "text/html") {
		t.Errorf("report %q does not describe the placeholder answer", waits[0])
	}
}

// TestLoginTimeoutNamesTheBootPlaceholder covers a controller that never
// finishes starting. The budget has to be what stops the wait, and the error
// has to say the answer was a placeholder — otherwise the operator is left
// with a JSON decode error and no hint that the controller never came up.
func TestLoginTimeoutNamesTheBootPlaceholder(t *testing.T) {
	handler, seen := bootingLogin(1<<30, nil) // never ready
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, Classic, time.Millisecond, 2*time.Millisecond)
	const budget = 300 * time.Millisecond
	start := time.Now()
	err := c.Login(waitCtx(t, budget), "admin", "admin")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Login against a controller that never finishes booting: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "never became ready") {
		t.Errorf("error %q does not say the controller never came up", err)
	}
	if elapsed < budget/2 {
		t.Errorf("Login gave up after %s of a %s budget; the context should be what stops it", elapsed, budget)
	}
	if got := seen(); got < 10 {
		t.Errorf("controller saw %d attempts in %s; retrying stopped early", got, elapsed)
	}
}

func TestUOSLoginFailure(t *testing.T) {
	f := newFakeUOS(t, "00:15:6d:00:00:01")
	c := New(f.server.URL, UniFiOS)
	ctx := waitCtx(t, 5*time.Second)

	// ucore puts the real reason in the body; the client error must surface
	// it for live debugging.
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
	c := New(f.server.URL, UniFiOS)

	err := c.Login(waitCtx(t, 5*time.Second), "admin", "admin")
	if err == nil {
		t.Fatal("Login with no CSRF token header: want error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "x-updated-csrf-token") {
		t.Errorf("Login error %q does not name the missing header", err)
	}
}

func TestUOSLoginAcceptsLegacyCSRFHeader(t *testing.T) {
	f := newFakeUOS(t, "00:15:6d:00:00:01")
	f.legacyCSRF = true
	c := New(f.server.URL, UniFiOS)
	if err := c.Login(waitCtx(t, 5*time.Second), "admin", "admin"); err != nil {
		t.Fatalf("Login with X-Csrf-Token: %v", err)
	}
	if got := c.csrf.get(); got != uosTokenV1 {
		t.Fatalf("stored CSRF token = %q, want %q", got, uosTokenV1)
	}
}

func TestUOSAuthedCallsNeedLogin(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeUOS(t, mac)
	c := New(f.server.URL, UniFiOS) // no Login: no cookie, no CSRF token
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
	c := New(f.server.URL, UniFiOS)
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
	c := New(f.server.URL, UniFiOS)
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
