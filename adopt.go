package emu

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
	"time"
)

// ClassicClient talks to a classic Network App controller API (:8443, cookie
// auth via /api/login). It exists so tests can drive an emulated device to
// adoption the way the controller UI does: login, devmgr adopt, then poll
// stat/device.
type ClassicClient struct {
	base string
	hc   *http.Client
}

// newSessionClient returns an http.Client with a cookie jar and TLS
// verification off because controllers ship self-signed certs; plain
// http:// URLs work too. The 15s timeout keeps a dead controller from
// hanging an adoption wait.
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

// NewClassicClient returns a client for the controller at baseURL.
func NewClassicClient(baseURL string) *ClassicClient {
	return &ClassicClient{
		base: strings.TrimRight(baseURL, "/"),
		hc:   newSessionClient(),
	}
}

// postJSON sends payload to base+path and errors on a non-200 status; hdr
// carries extra request headers (UOS sends its CSRF token on every authed
// call). The error carries the response body (capped at 512 bytes): the
// controller puts the real failure reason there (api.err.*), and without it
// a failed adopt is undebuggable against a live controller.
func postJSON(ctx context.Context, hc *http.Client, base, path string, hdr http.Header, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
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
			path, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("emu: POST %s: read response: %w", path, err)
	}
	return checkAPIEnvelope("POST "+path, reply)
}

type apiEnvelope struct {
	Meta struct {
		RC  string `json:"rc"`
		Msg string `json:"msg"`
	} `json:"meta"`
}

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

// Login authenticates against /api/login; the session cookie rides in the
// jar from then on. Non-200 (bad credentials) is an error.
func (c *ClassicClient) Login(ctx context.Context, user, pass string) error {
	return postJSON(ctx, c.hc, c.base, "/api/login", nil, map[string]string{
		"username": user,
		"password": pass,
	})
}

// Adopt issues the devmgr adopt command for mac in site, the same call the
// controller UI makes when the user clicks Adopt.
func (c *ClassicClient) Adopt(ctx context.Context, site, mac string) error {
	return postJSON(ctx, c.hc, c.base, "/api/s/"+site+"/cmd/devmgr", nil, map[string]string{
		"cmd": "adopt",
		"mac": mac,
	})
}

// Device is the subset of a stat/device document the adoption flow reads;
// both ClassicClient and UOSClient decode it. The documents carry many
// more fields; they are ignored.
type Device struct {
	MAC     string `json:"mac"`
	State   int    `json:"state"` // 1=connected, 2=pending, 7=adopt-failed
	Adopted bool   `json:"adopted"`
	Model   string `json:"model"`
	IP      string `json:"ip"`
	Name    string `json:"name"`
}

// DeviceByMAC returns the stat/device doc for mac in site, or a "device not
// found" error when the controller does not list it.
func (c *ClassicClient) DeviceByMAC(ctx context.Context, site, mac string) (Device, error) {
	return deviceByMAC(ctx, c.hc, c.base+"/api/s/"+site+"/stat/device", nil, mac)
}

// deviceByMAC GETs url (a stat/device endpoint) and returns the doc for
// mac, or a "device not found" error when the controller does not list it;
// hdr carries extra request headers (see postJSON).
func deviceByMAC(ctx context.Context, hc *http.Client, url string, hdr http.Header, mac string) (Device, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Device{}, err
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Device{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Device{}, fmt.Errorf("emu: GET stat/device: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var reply struct {
		apiEnvelope
		Data []Device `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return Device{}, fmt.Errorf("emu: decode stat/device: %w", err)
	}
	if reply.Meta.RC != "" && reply.Meta.RC != "ok" {
		msg := reply.Meta.Msg
		if msg == "" {
			msg = "controller returned meta.rc=" + reply.Meta.RC
		}
		return Device{}, fmt.Errorf("emu: GET stat/device: %s", msg)
	}
	for _, d := range reply.Data {
		if strings.EqualFold(d.MAC, mac) {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf("emu: device %s not found", mac)
}

// WaitAdopted polls stat/device every 2s until the device reports state 1
// and adopted. On ctx timeout it returns the last seen device and an error
// naming it plus the last poll error, so a stalled adoption says where it
// stalled.
func (c *ClassicClient) WaitAdopted(ctx context.Context, site, mac string) (Device, error) {
	return waitAdopted(ctx, c, site, mac)
}

// deviceFinder is the piece of ClassicClient and UOSClient that waitAdopted
// needs: fetch one device's doc by MAC.
type deviceFinder interface {
	DeviceByMAC(ctx context.Context, site, mac string) (Device, error)
}

func waitAdopted(ctx context.Context, f deviceFinder, site, mac string) (Device, error) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var last Device
	var lastErr error
	for {
		d, err := f.DeviceByMAC(ctx, site, mac)
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
