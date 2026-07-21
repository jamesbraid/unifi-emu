package unifiemu

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

// informOnce sends one inform packet and applies the controller's reply.
//
// HTTP 404 is the expected answer while the device is pending — the
// controller has nothing queued for a device nobody adopted — so it is
// neither an error nor worth a log line.
//
// The ADOPTING -> CONNECTED transition happens only when a reply arrives to
// an inform that was sent adopted: the state is sampled before applyResponse,
// not after. Sampling after would flip CONNECTED the instant set-adopt lands,
// before the controller has ever seen an adopt-key inform — ADOPTING would be
// unobservable and CONNECTED would claim a handshake that has not finished.
func (d *device) informOnce(ctx context.Context) {
	d.mu.Lock()
	key := d.key
	url := d.informURL
	d.mu.Unlock()

	enc, err := (&inform.Packet{MAC: d.macHeader(), Payload: d.buildPayload()}).Encode(key)
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
	adopting := d.state == StateAdopting
	d.mu.Unlock()
	d.applyResponse(dec.Payload)
	if adopting {
		d.mu.Lock()
		if d.adopted && d.state == StateAdopting {
			d.state = StateConnected
			log.Printf("[%s] adoption handshake complete -> CONNECTED", d.spec.MAC)
		}
		d.mu.Unlock()
	}
}
