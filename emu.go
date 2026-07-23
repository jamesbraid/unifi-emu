package emu

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// Option customizes an Emu fleet.
type Option func(*Emu)

// WithInformInterval sets the inform interval every added device starts with.
// Controller responses can still retune it per device later.
func WithInformInterval(d time.Duration) Option {
	return func(e *Emu) { e.interval = d }
}

// Emu is a fleet of emulated UniFi devices informing one controller.
type Emu struct {
	informURL string
	interval  time.Duration

	mu      sync.Mutex
	devices map[string]*device // keyed by normalized MAC
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New builds a fleet whose devices will inform informURL.
func New(informURL string, opts ...Option) *Emu {
	e := &Emu{
		informURL: informURL,
		interval:  10 * time.Second,
		devices:   map[string]*device{},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// normalizeMAC returns the canonical lowercase colon form of s, so fleet
// lookups match however the caller spelled the address.
func normalizeMAC(s string) (string, error) {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return "", fmt.Errorf("bad MAC %q: %w", s, err)
	}
	if len(hw) != 6 {
		return "", fmt.Errorf("bad MAC %q: want a 6-byte address, got %d bytes", s, len(hw))
	}
	return hw.String(), nil
}

// Add validates specs and adds them to the fleet. MACs are normalized before
// keying, so the same device added twice errors however it was spelled. The
// first invalid spec aborts the call; earlier specs stay added. Add errors
// once Start has been called: a running fleet is fixed, and devices added
// after Start would never be launched.
func (e *Emu) Add(specs ...DeviceSpec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return fmt.Errorf("emu: cannot Add after Start")
	}
	for _, spec := range specs {
		mac, err := normalizeMAC(spec.MAC)
		if err != nil {
			return err
		}
		if _, dup := e.devices[mac]; dup {
			return fmt.Errorf("emu: duplicate MAC %q", mac)
		}
		spec.MAC = mac
		d, err := newDevice(spec, e.informURL)
		if err != nil {
			return err
		}
		d.interval = e.interval
		e.devices[mac] = d
	}
	return nil
}

// Start launches one inform goroutine per device, all tied to ctx. Start is
// one-shot: a second Start errors "emu: already started" even after Stop — that is
// intended, build a fresh fleet with New to restart. Starting an empty fleet
// errors rather than welding it shut: once started, Add rejects new devices.
func (e *Emu) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return fmt.Errorf("emu: already started")
	}
	if len(e.devices) == 0 {
		return fmt.Errorf("emu: no devices added")
	}
	if e.interval <= 0 {
		return fmt.Errorf("emu: inform interval must be positive, got %s", e.interval)
	}
	e.started = true
	ctx, e.cancel = context.WithCancel(ctx)
	for _, d := range e.devices {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			d.run(ctx)
		}()
	}
	return nil
}

// State reports the adoption state of one device, ok=false when mac is
// unknown to the fleet or unparseable.
func (e *Emu) State(mac string) (DeviceState, bool) {
	norm, err := normalizeMAC(mac)
	if err != nil {
		return 0, false
	}
	e.mu.Lock()
	d, ok := e.devices[norm]
	e.mu.Unlock()
	if !ok {
		return 0, false
	}
	return d.State(), true
}

// WaitState polls every 10ms until mac reaches want or ctx is done. The
// timeout error names the last observed state so a stalled adoption tells
// the caller where it stalled.
func (e *Emu) WaitState(ctx context.Context, mac string, want DeviceState) error {
	got, ok := e.State(mac)
	if !ok {
		return fmt.Errorf("unknown device %q", mac)
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if got == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to reach %s: %w (last state %s)",
				mac, want, ctx.Err(), got)
		case <-tick.C:
			got, ok = e.State(mac)
			if !ok {
				return fmt.Errorf("unknown device %q", mac)
			}
		}
	}
}

// Stop cancels every device loop and waits for them to return. It is safe to
// call more than once.
func (e *Emu) Stop() {
	e.mu.Lock()
	cancel := e.cancel
	e.cancel = nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	e.wg.Wait()
}
