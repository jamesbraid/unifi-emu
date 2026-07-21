package unifiemu

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

// NewClassicClient returns a client for the controller at baseURL. TLS
// verification is off because controllers ship self-signed certs; plain
// http:// URLs work too. The 15s timeout keeps a dead controller from
// hanging an adoption wait.
func NewClassicClient(baseURL string) *ClassicClient {
	jar, _ := cookiejar.New(nil) // nil options never error
	return &ClassicClient{
		base: strings.TrimRight(baseURL, "/"),
		hc: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			Jar:     jar,
			Timeout: 15 * time.Second,
		},
	}
}

// postJSON sends payload to path and errors on a non-200 status.
func (c *ClassicClient) postJSON(ctx context.Context, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection is reused
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unifiemu: POST %s: %s", path, resp.Status)
	}
	return nil
}

// Login authenticates against /api/login; the session cookie rides in the
// jar from then on. Non-200 (bad credentials) is an error.
func (c *ClassicClient) Login(ctx context.Context, user, pass string) error {
	return c.postJSON(ctx, "/api/login", map[string]string{
		"username": user,
		"password": pass,
	})
}

// Adopt issues the devmgr adopt command for mac in site, the same call the
// controller UI makes when the user clicks Adopt.
func (c *ClassicClient) Adopt(ctx context.Context, site, mac string) error {
	return c.postJSON(ctx, "/api/s/"+site+"/cmd/devmgr", map[string]string{
		"cmd": "adopt",
		"mac": mac,
	})
}

// Device is the subset of a classic stat/device document the adoption flow
// reads. The documents carry many more fields; they are ignored.
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/api/s/"+site+"/stat/device", nil)
	if err != nil {
		return Device{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Device{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return Device{}, fmt.Errorf("unifiemu: GET stat/device: %s", resp.Status)
	}
	var reply struct {
		Data []Device `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return Device{}, fmt.Errorf("unifiemu: decode stat/device: %w", err)
	}
	for _, d := range reply.Data {
		if strings.EqualFold(d.MAC, mac) {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf("unifiemu: device %s not found", mac)
}

// WaitAdopted polls stat/device every 2s until the device reports state 1
// and adopted. On ctx timeout it returns the last seen device and an error
// naming it plus the last poll error, so a stalled adoption says where it
// stalled.
func (c *ClassicClient) WaitAdopted(ctx context.Context, site, mac string) (Device, error) {
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
			return last, fmt.Errorf("unifiemu: waiting for %s adoption: %w (last device %+v, last error %v)",
				mac, ctx.Err(), last, lastErr)
		case <-tick.C:
		}
	}
}
