package emu

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/jamesbraid/unifi-emu/inform"
)

// State returns the device's current adoption state.
func (d *device) State() DeviceState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// run informs the controller, then sleeps for the current inform interval,
// until ctx is cancelled. The interval is re-read every cycle because
// controller responses (noop interval) can change it.
func (d *device) run(ctx context.Context) {
	for {
		d.informOnce(ctx)
		d.mu.Lock()
		interval := d.interval
		d.mu.Unlock()
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// informStatusHeartbeat is how long a repeating inform status stays silent
// before noteStatus logs it again. Transitions alone are enough evidence for
// a device that gets adopted promptly, but a device the controller never
// lists 404s until the test gives up, and silence there is indistinguishable
// from a wedged emulator. A var, so tests can shorten it.
var informStatusHeartbeat = 60 * time.Second

// noteStatus records the inform's HTTP status and logs transitions plus a
// periodic heartbeat, never every inform: a pending device 404s every
// interval, and logging each one would bury the log lines that matter. The
// first 404 announces "pending, nothing queued"; the first 200 after a run of
// 404s reports the run length, which is the evidence that 404 meant "nothing
// queued" rather than a protocol mismatch. A run that outlives
// informStatusHeartbeat is relogged with its length, so a device stuck
// pending keeps saying so instead of going quiet.
func (d *device) noteStatus(status int) {
	now := time.Now()
	d.mu.Lock()
	prev, run := d.lastStatus, d.statusRun
	if status == d.lastStatus {
		d.statusRun++
		run = d.statusRun
		since := now.Sub(d.lastStatusLog)
		if since < informStatusHeartbeat {
			d.mu.Unlock()
			return
		}
		d.lastStatusLog = now
		d.mu.Unlock()
		log.Printf("[%s] inform: still HTTP %d (%d in a row, %s)",
			d.spec.MAC, status, run, since.Round(time.Second))
		return
	}
	d.lastStatus, d.statusRun, d.lastStatusLog = status, 1, now
	d.mu.Unlock()
	// Logging after the unlock: log writes can block on I/O and must not
	// stall State readers (same pattern as applyResponse).
	switch {
	case status == http.StatusNotFound:
		log.Printf("[%s] inform: HTTP %d (nothing queued)", d.spec.MAC, status)
	case prev == 0:
		log.Printf("[%s] inform: HTTP %d", d.spec.MAC, status)
	default:
		log.Printf("[%s] inform: HTTP %d after %d x %d", d.spec.MAC, status, run, prev)
	}
}

// informOnce sends one inform packet and applies the controller's reply.
//
// HTTP 404 is the expected answer while the device is pending — the
// controller has nothing queued for a device nobody adopted — so it is
// not an error; noteStatus logs only the transitions into and out of a
// 404 run, keeping the run itself silent.
//
// The ADOPTING -> CONNECTED transition happens only when a reply arrives to
// an inform that was sent adopted: the state is sampled before applyResponse,
// not after. Sampling after would flip CONNECTED the instant set-adopt lands,
// before the controller has ever seen an adopt-key inform — ADOPTING would be
// unobservable and CONNECTED would claim a handshake that has not finished.
func (d *device) informOnce(ctx context.Context) {
	now := time.Now()
	d.mu.Lock()
	enc, err := d.session.EncodeInform(now)
	url := d.session.InformURL()
	key := d.session.AuthKey()
	d.mu.Unlock()
	if err != nil {
		log.Printf("[%s] encode inform: %v", d.spec.MAC, err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(enc))
	if err != nil {
		log.Printf("[%s] build inform request: %v", d.spec.MAC, err)
		return
	}
	req.Header.Set("Content-Type", "application/x-binary")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() == nil { // cancellation mid-flight is shutdown, not an error
			log.Printf("[%s] inform POST %s: %v", d.spec.MAC, url, err)
		}
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[%s] read inform response: %v", d.spec.MAC, err)
		return
	}
	d.noteStatus(resp.StatusCode)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return // pending, nothing queued: expected and silent
	case resp.StatusCode != http.StatusOK:
		log.Printf("[%s] inform got %s: %s", d.spec.MAC, resp.Status, body)
		return
	case len(body) == 0:
		return
	}

	dec, err := inform.Decode(body, key)
	if err != nil {
		log.Printf("[%s] decode inform response: %v", d.spec.MAC, err)
		return
	}
	d.mu.Lock()
	wasAdopting := d.state == StateAdopting
	d.mu.Unlock()
	d.applyResponse(dec.Payload)
	if wasAdopting {
		d.mu.Lock()
		if d.session.Adopted() && d.state == StateAdopting {
			d.state = StateConnected
			log.Printf("[%s] adoption handshake complete -> CONNECTED", d.spec.MAC)
		}
		d.mu.Unlock()
	}
}
