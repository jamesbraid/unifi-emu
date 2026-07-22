package emu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// csrfToken holds the ucore CSRF token. ucore rotates it mid-session and
// returns the replacement on ordinary responses, so the transport writes it
// while requests read it: guard with a mutex.
type csrfToken struct {
	mu  sync.RWMutex
	tok string
}

func (t *csrfToken) get() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.tok
}

func (t *csrfToken) set(tok string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tok = tok
}

// csrfSniffer is an http.RoundTripper that watches every response for the
// headers ucore uses to rotate the CSRF token — X-Updated-Csrf-Token first,
// X-Csrf-Token as fallback — and stores the latest. go-unifi tracks the
// token per-response the same way; a client that pins the login-time token
// starts getting 403s as soon as ucore rotates it.
type csrfSniffer struct {
	base http.RoundTripper
	tok  *csrfToken
}

func (s *csrfSniffer) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	tok := resp.Header.Get("X-Updated-Csrf-Token")
	if tok == "" {
		tok = resp.Header.Get("X-Csrf-Token")
	}
	if tok != "" {
		s.tok.set(tok)
	}
	return resp, nil
}

// UOSClient talks to a UniFi OS controller through the ucore proxy (:443).
// Login at /api/auth/login yields a session cookie plus a CSRF token in the
// x-updated-csrf-token response header, and every call under /proxy/network
// must carry that token in X-CSRF-Token or ucore answers 403. The token
// rotates mid-session; the transport follows the rotation (see csrfSniffer).
// The Network App API behind the proxy is the classic one, so the paths
// below mirror ClassicClient's under the /proxy/network prefix.
type UOSClient struct {
	base string
	hc   *http.Client
	csrf *csrfToken // empty until Login
}

// NewUOSClient returns a client for the UniFi OS controller at baseURL; see
// newSessionClient for the TLS and timeout rationale.
func NewUOSClient(baseURL string) *UOSClient {
	c := &UOSClient{
		base: strings.TrimRight(baseURL, "/"),
		csrf: &csrfToken{},
	}
	hc := newSessionClient()
	hc.Transport = &csrfSniffer{base: hc.Transport, tok: c.csrf}
	c.hc = hc
	return c
}

// Login authenticates against /api/auth/login. The session cookie rides in
// the jar; the CSRF token comes back in the x-updated-csrf-token response
// header. A 200 without that header is still an error: without the token
// every proxied call would 403, so a tokenless login is no login.
func (c *UOSClient) Login(ctx context.Context, user, pass string) error {
	body, err := json.Marshal(map[string]string{
		"username": user,
		"password": pass,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("emu: POST /api/auth/login: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	tok := resp.Header.Get("x-updated-csrf-token")
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection is reused
	if tok == "" {
		return fmt.Errorf("emu: login 200 but no x-updated-csrf-token header")
	}
	c.csrf.set(tok)
	return nil
}

// authHeader carries the token ucore demands on every proxied call.
func (c *UOSClient) authHeader() http.Header {
	return http.Header{"X-CSRF-Token": []string{c.csrf.get()}}
}

// Adopt issues the devmgr adopt command for mac in site through the proxy,
// the same call the Network App UI makes when the user clicks Adopt.
func (c *UOSClient) Adopt(ctx context.Context, site, mac string) error {
	return postJSON(ctx, c.hc, c.base, "/proxy/network/api/s/"+site+"/cmd/devmgr",
		c.authHeader(), map[string]string{
			"cmd": "adopt",
			"mac": mac,
		})
}

// DeviceByMAC returns the stat/device doc for mac in site, or a "device not
// found" error when the controller does not list it.
func (c *UOSClient) DeviceByMAC(ctx context.Context, site, mac string) (Device, error) {
	return deviceByMAC(ctx, c.hc,
		c.base+"/proxy/network/api/s/"+site+"/stat/device", c.authHeader(), mac)
}

// WaitAdopted polls stat/device every 2s until the device reports state 1
// and adopted; same semantics as ClassicClient.WaitAdopted.
func (c *UOSClient) WaitAdopted(ctx context.Context, site, mac string) (Device, error) {
	return waitAdopted(ctx, c, site, mac)
}
