// Package adopt drives a device to adoption against a real controller the
// way the controller UI does — login, devmgr adopt, poll stat/device.
//
// It is internal on purpose. The exported ClassicClient/UOSClient this code
// grew from were removed in a53d613 because they had no non-test callers and
// pulled go-retryablehttp into every importer of package emu. As an internal
// package the emulator's own CLI can adopt while importers of package emu
// still cannot reach this code and still do not link its dependencies.
package adopt

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"

	retryablehttp "github.com/hashicorp/go-retryablehttp"
)

// Client drives adoption against either dialect. hc wraps a cookie-jar
// session in go-retryablehttp so login survives UniFi OS's global login
// rate-limiting (429 + 5xx are retried, Retry-After honored). csrf is
// empty until Login and only meaningful for the UniFiOS dialect.
type Client struct {
	base    string
	dialect Dialect
	hc      *http.Client
	csrf    *csrfToken
}

// newSessionClient returns an http.Client with a cookie jar and TLS
// verification off because controllers ship self-signed certs; plain
// http:// URLs work too. The 15s timeout keeps a dead controller from
// hanging a single request.
func newSessionClient() *http.Client {
	jar, _ := cookiejar.New(nil) // nil options never error
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Jar:     jar,
		Timeout: 15 * time.Second,
	}
}

// New returns a client for the controller at baseURL. The cookie-jar
// session's transport is wrapped in a csrfSniffer (harmless for Classic — it
// simply never sees a token) and the whole thing in go-retryablehttp; the
// cookie jar and CSRF sniffing stay on the inner client, which retryablehttp
// drives per attempt.
func New(baseURL string, d Dialect) *Client {
	c := &Client{
		base:    strings.TrimRight(baseURL, "/"),
		dialect: d,
		csrf:    &csrfToken{},
	}
	inner := newSessionClient()
	inner.Transport = &csrfSniffer{base: inner.Transport, tok: c.csrf}

	rc := retryablehttp.NewClient()
	rc.HTTPClient = inner
	rc.Logger = nil // no log spam
	rc.RetryMax = 8
	c.hc = rc.StandardClient()
	return c
}

// loginPath is where each dialect authenticates.
func (c *Client) loginPath() string {
	if c.dialect == UniFiOS {
		return "/api/auth/login"
	}
	return "/api/login"
}

// apiURL builds the full URL for a Network App endpoint (e.g. "stat/device")
// in site, adding the /proxy/network prefix ucore puts in front of the
// proxied Network App API.
func (c *Client) apiURL(site, endpoint string) string {
	prefix := ""
	if c.dialect == UniFiOS {
		prefix = "/proxy/network"
	}
	return c.base + prefix + "/api/s/" + site + "/" + endpoint
}

// authHeader carries the token ucore demands on every proxied call; the
// classic API authenticates by cookie alone, so it gets no extra header.
func (c *Client) authHeader() http.Header {
	if c.dialect == UniFiOS {
		return http.Header{"X-CSRF-Token": {c.csrf.get()}}
	}
	return nil
}

// Login authenticates against the dialect's login path; the session cookie
// rides in the jar from then on. Classic verifies the meta.rc==ok envelope;
// UniFiOS also requires the CSRF token the csrfSniffer captures from the
// login response header (a 200 without it would 403 every proxied call, so a
// tokenless login is no login). Non-200 (bad credentials) is an error
// carrying the response body, where the controller puts the real reason.
func (c *Client) Login(ctx context.Context, user, pass string) error {
	body, err := json.Marshal(map[string]string{"username": user, "password": pass})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+c.loginPath(), bytes.NewReader(body))
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
		return fmt.Errorf("emu: POST %s: HTTP %d: %s",
			c.loginPath(), resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	if c.dialect == UniFiOS {
		_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection is reused; the token rode in on a header
		if c.csrf.get() == "" {
			return fmt.Errorf("emu: login 200 but no X-Updated-Csrf-Token or X-Csrf-Token header")
		}
		return nil
	}
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("emu: POST %s: read response: %w", c.loginPath(), err)
	}
	return checkAPIEnvelope("POST "+c.loginPath(), reply)
}

// Adopt issues the devmgr adopt command for mac in site, the same call the
// controller UI makes when the user clicks Adopt.
func (c *Client) Adopt(ctx context.Context, site, mac string) error {
	return postJSON(ctx, c.hc, c.apiURL(site, "cmd/devmgr"), c.authHeader(), map[string]string{
		"cmd": "adopt",
		"mac": mac,
	})
}

// DeviceByMAC returns the stat/device doc for mac in site, or a "device not
// found" error when the controller does not list it.
func (c *Client) DeviceByMAC(ctx context.Context, site, mac string) (Device, error) {
	return deviceByMAC(ctx, c.hc, c.apiURL(site, "stat/device"), c.authHeader(), mac)
}

// Devices returns every stat/device doc in site. A device that never appears
// reports only "not found", which cannot distinguish a controller that
// dropped this one device from a controller that listed nothing at all; the
// full list is what tells those apart after the fact.
func (c *Client) Devices(ctx context.Context, site string) ([]Device, error) {
	return devices(ctx, c.hc, c.apiURL(site, "stat/device"), c.authHeader())
}

// WaitAdopted polls stat/device every 2s until the device reports state 1
// and adopted. On ctx timeout it returns the last seen device and an error
// naming it plus the last poll error, so a stalled adoption says where it
// stalled.
func (c *Client) WaitAdopted(ctx context.Context, site, mac string) (Device, error) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var last Device
	var lastErr error
	for {
		d, err := c.DeviceByMAC(ctx, site, mac)
		if err != nil {
			lastErr = err
		} else {
			last, lastErr = d, nil
			if d.State == 1 && d.Adopted {
				return d, nil
			}
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("emu: waiting for %s adoption: %w (last device %+v, last error %v)",
				mac, ctx.Err(), last, lastErr)
		case <-tick.C:
		}
	}
}

// Device is the subset of a stat/device document the adoption flow reads.
// The documents carry many more fields; they are ignored.
type Device struct {
	MAC string `json:"mac"`
	// State is the controller's device state. A healthy classic adoption
	// was measured to run 2 -> 7 -> 5 -> 1, so despite 7 being widely
	// documented as "adopt-failed" it is a step on the happy path here
	// (carrying adopted=true) and must not be read as a verdict: treating
	// it as terminal fails an adoption that would have completed.
	State   int    `json:"state"` // 1=connected, 2=pending, 5+7=mid-adoption
	Adopted bool   `json:"adopted"`
	Model   string `json:"model"`
	IP      string `json:"ip"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// postJSON sends payload to url and errors on a non-200 status; hdr carries
// extra request headers (UniFiOS sends its CSRF token on every authed call).
// The error carries the response body (capped at 512 bytes): the controller
// puts the real failure reason there (api.err.*), and without it a failed
// adopt is undebuggable against a live controller.
func postJSON(ctx context.Context, hc *http.Client, url string, hdr http.Header, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("emu: POST %s: HTTP %d: %s",
			url, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("emu: POST %s: read response: %w", url, err)
	}
	return checkAPIEnvelope("POST "+url, reply)
}

type apiEnvelope struct {
	Meta struct {
		RC  string `json:"rc"`
		Msg string `json:"msg"`
	} `json:"meta"`
}

// checkAPIEnvelope errors when the controller's meta.rc envelope reports
// anything other than "ok", surfacing meta.msg for live debugging.
func checkAPIEnvelope(operation string, body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var reply apiEnvelope
	if err := json.Unmarshal(body, &reply); err != nil {
		return fmt.Errorf("emu: %s: decode response: %w", operation, err)
	}
	if reply.Meta.RC != "" && reply.Meta.RC != "ok" {
		msg := reply.Meta.Msg
		if msg == "" {
			msg = "controller returned meta.rc=" + reply.Meta.RC
		}
		return fmt.Errorf("emu: %s: %s", operation, msg)
	}
	return nil
}

// devices GETs url (a stat/device endpoint) and returns every doc the
// controller lists; hdr carries extra request headers (see postJSON).
func devices(ctx context.Context, hc *http.Client, url string, hdr http.Header) ([]Device, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("emu: GET stat/device: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var reply struct {
		apiEnvelope
		Data []Device `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, fmt.Errorf("emu: decode stat/device: %w", err)
	}
	if reply.Meta.RC != "" && reply.Meta.RC != "ok" {
		msg := reply.Meta.Msg
		if msg == "" {
			msg = "controller returned meta.rc=" + reply.Meta.RC
		}
		return nil, fmt.Errorf("emu: GET stat/device: %s", msg)
	}
	return reply.Data, nil
}

// deviceByMAC returns the doc for mac, or a "device not found" error when the
// controller does not list it.
func deviceByMAC(ctx context.Context, hc *http.Client, url string, hdr http.Header, mac string) (Device, error) {
	list, err := devices(ctx, hc, url, hdr)
	if err != nil {
		return Device{}, err
	}
	for _, d := range list {
		if strings.EqualFold(d.MAC, mac) {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf("emu: device %s not found", mac)
}

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
// starts getting 403s as soon as ucore rotates it. Classic controllers
// never send these headers, so for that dialect the sniffer is a no-op.
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
