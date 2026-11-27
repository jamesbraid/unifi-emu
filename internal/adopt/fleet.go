package adopt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Default polling cadences. Both are tuned for a real controller: it lists a
// freshly informing device within a couple of seconds, and it rate-limits
// repeated adopt commands, so a rejected adopt waits considerably longer
// before trying again.
const (
	defaultPollInterval  = 2 * time.Second
	defaultRetryInterval = 10 * time.Second
	defaultStuckAfter    = 60 * time.Second
	defaultMaxResets     = 3
)

// stateInformError is the controller's INFORM_ERROR device state (the device
// enum in the Network application). A freshly booted controller can accept an
// adopt, mark the device adopted, and then hold it here, answering every
// inform 404 without ever sending the new key.
const stateInformError = 9

// Options tunes Fleet. The zero value is usable: the intervals fall back to
// the defaults above and the hooks are all optional.
type Options struct {
	// PollInterval is how often the pending-document wait re-reads
	// stat/device. RetryInterval is how long to wait after the controller
	// rejects an adopt as premature.
	PollInterval  time.Duration
	RetryInterval time.Duration

	// RequirePending makes an already-adopted device an error instead of a
	// no-op. Tests that boot a fresh controller set this: there, a connected
	// device means the fixture is dirty. The emulator leaves it off, so
	// restarting against a controller that still holds the fleet succeeds.
	RequirePending bool

	// OnPending and OnConnected report each device as it reaches the
	// controller's pending list and as it finishes adopting.
	OnPending   func(Device)
	OnConnected func(Device)

	// StuckAfter is how long a device may sit adopted in INFORM_ERROR before
	// its adopt is sent again. MaxResets bounds how often that happens per
	// device. Zero values take the defaults.
	StuckAfter time.Duration
	MaxResets  int

	// OnReset reports each re-sent adopt, with its 1-based attempt number.
	OnReset func(d Device, attempt int)

	// Check runs on every poll. A non-nil error aborts immediately, wrapped
	// in the returned error. It exists so a caller that knows the devices
	// have stopped informing — the emulator container died, say — fails at
	// once rather than waiting out the deadline with nothing to say.
	Check func() error
}

func (o Options) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return defaultPollInterval
}

func (o Options) retryInterval() time.Duration {
	if o.RetryInterval > 0 {
		return o.RetryInterval
	}
	return defaultRetryInterval
}

func (o Options) stuckAfter() time.Duration {
	if o.StuckAfter > 0 {
		return o.StuckAfter
	}
	return defaultStuckAfter
}

func (o Options) maxResets() int {
	if o.MaxResets > 0 {
		return o.MaxResets
	}
	return defaultMaxResets
}

// Fleet drives every MAC in macs to a connected, adopted device on the
// controller: wait for the pending document, issue the adopt, wait for
// state 1.
//
// Adopts go out one device at a time. That is not caution — controllers
// reject bursts of adopts, and reject documents that are only seconds old, so
// adopting in parallel is slower and flakier than adopting in sequence. The
// wait for state 1 is shared, though: one device the controller has stalled
// must not use up the budget the others need, so every adopted device is
// watched together, and a device held in INFORM_ERROR has its adopt sent
// again, a bounded number of times.
//
// The whole run is bounded by ctx. A failure names every device that did not
// connect, with its last state and how many times its adopt was re-sent.
func Fleet(ctx context.Context, c *Client, site string, macs []string, opts Options) error {
	var watch []*watched
	for _, mac := range macs {
		device, adopted, err := waitPending(ctx, c, site, mac, opts)
		if err != nil {
			return err
		}
		if adopted {
			if opts.RequirePending {
				return fmt.Errorf("%s is already adopted (state %d) on a controller that should not know it",
					mac, device.State)
			}
			if opts.OnConnected != nil {
				opts.OnConnected(device)
			}
			continue
		}
		if opts.OnPending != nil {
			opts.OnPending(device)
		}
		if err := adoptWithRetry(ctx, c, site, mac, opts); err != nil {
			return err
		}
		watch = append(watch, &watched{mac: mac, last: device})
	}
	return waitFleetAdopted(ctx, c, site, watch, opts)
}

// watched is one adopted device on its way to state 1.
type watched struct {
	mac       string
	last      Device
	lastErr   error
	connected bool
	resets    int
	gaveUp    bool
	stuckFrom time.Time // when it was first seen in INFORM_ERROR, zero if not
}

func (w *watched) String() string {
	if w.lastErr != nil && w.last.MAC == "" {
		return fmt.Sprintf("%s never read back (%v)", w.mac, w.lastErr)
	}
	return fmt.Sprintf("%s state=%d adopted=%v resets=%d", w.mac, w.last.State, w.last.Adopted, w.resets)
}

// waitFleetAdopted polls every adopted device until each is connected or has
// used up its re-sent adopts.
func waitFleetAdopted(ctx context.Context, c *Client, site string, fleet []*watched, opts Options) error {
	if len(fleet) == 0 {
		return nil
	}
	tick := time.NewTicker(opts.pollInterval())
	defer tick.Stop()
	for {
		settled := true
		for _, w := range fleet {
			if w.connected || w.gaveUp {
				continue
			}
			settled = false
			pollWatched(ctx, c, site, w, opts)
		}
		if settled {
			break
		}
		if err := checkAlive(opts); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fleetFailure(fleet, ctx.Err())
		case <-tick.C:
		}
	}
	for _, w := range fleet {
		if !w.connected {
			return fleetFailure(fleet, nil)
		}
	}
	return nil
}

// pollWatched reads one device back and acts on what it shows.
func pollWatched(ctx context.Context, c *Client, site string, w *watched, opts Options) {
	d, err := c.DeviceByMAC(ctx, site, w.mac)
	if err != nil {
		w.lastErr = err
		return
	}
	w.last, w.lastErr = d, nil
	if d.State == 1 && d.Adopted {
		w.connected = true
		if opts.OnConnected != nil {
			opts.OnConnected(d)
		}
		return
	}
	if d.State != stateInformError || !d.Adopted {
		w.stuckFrom = time.Time{}
		return
	}
	if w.stuckFrom.IsZero() {
		w.stuckFrom = time.Now()
		return
	}
	if time.Since(w.stuckFrom) < opts.stuckAfter() {
		return
	}
	if w.resets >= opts.maxResets() {
		w.gaveUp = true
		return
	}
	w.resets++
	w.stuckFrom = time.Time{}
	if opts.OnReset != nil {
		opts.OnReset(d, w.resets)
	}
	// A failed re-send is not fatal: the device is still adopted, and the
	// next stuck interval tries again until the bound is reached.
	if err := c.Adopt(ctx, site, w.mac); err != nil {
		w.lastErr = err
	}
}

// fleetFailure names every device that did not connect. cause is the context
// error when the deadline ended the wait, nil when re-sends ran out.
func fleetFailure(fleet []*watched, cause error) error {
	var stuck []string
	for _, w := range fleet {
		if !w.connected {
			stuck = append(stuck, w.String())
		}
	}
	msg := fmt.Sprintf("%d of %d adopted devices never connected: %s",
		len(stuck), len(fleet), strings.Join(stuck, "; "))
	if cause != nil {
		return fmt.Errorf("%s: %w", msg, cause)
	}
	return errors.New(msg)
}

// waitPending polls until the controller lists mac, reporting whether it is
// already connected and adopted. A device that never appears is the
// signature of an emulator that produced no informs, so the error says
// exactly that rather than blaming the adopt.
func waitPending(ctx context.Context, c *Client, site, mac string, opts Options) (Device, bool, error) {
	tick := time.NewTicker(opts.pollInterval())
	defer tick.Stop()
	var last Device
	var lastErr error
	seen := false
	for {
		device, err := c.DeviceByMAC(ctx, site, mac)
		if err == nil {
			last, seen, lastErr = device, true, nil
			if device.State == 1 && device.Adopted {
				return device, true, nil
			}
			if device.State == 2 {
				return device, false, nil
			}
		} else {
			lastErr = err
		}
		if err := checkAlive(opts); err != nil {
			return last, false, err
		}
		select {
		case <-ctx.Done():
			if seen {
				return last, false, fmt.Errorf(
					"%s never reached pending: %w (last state %d adopted=%v)",
					mac, ctx.Err(), last.State, last.Adopted)
			}
			return last, false, fmt.Errorf(
				"%s never reached pending: %w (never listed, last error %v)",
				mac, ctx.Err(), lastErr)
		case <-tick.C:
		}
	}
}

// adoptWithRetry issues the adopt command, retrying only the two answers that
// improve with time: CannotAdopt, the controller's way of saying the device
// document is too young, and the boot placeholder from a Network App that has
// not finished starting — which on ucore outlives the login, because that
// lands against the OS in front of it. Any other reason will not improve with
// time, so it fails at once and carries the controller's own message.
func adoptWithRetry(ctx context.Context, c *Client, site, mac string, opts Options) error {
	tick := time.NewTicker(opts.retryInterval())
	defer tick.Stop()
	for {
		err := c.Adopt(ctx, site, mac)
		if err == nil {
			return nil
		}
		var booting notReady
		if !errors.As(err, &booting) && !strings.Contains(strings.ToLower(err.Error()), "cannotadopt") {
			return fmt.Errorf("adopt %s: %w", mac, err)
		}
		// CannotAdopt can also mean the adopt already landed and the
		// controller is refusing a second one; the document settles that.
		if device, lookupErr := c.DeviceByMAC(ctx, site, mac); lookupErr == nil && device.Adopted {
			return nil
		}
		if checkErr := checkAlive(opts); checkErr != nil {
			return checkErr
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s never adopted: %w (last adopt error %v)", mac, ctx.Err(), err)
		case <-tick.C:
		}
	}
}

// checkAlive runs the caller's liveness hook, if any.
func checkAlive(opts Options) error {
	if opts.Check == nil {
		return nil
	}
	if err := opts.Check(); err != nil {
		return fmt.Errorf("adoption abandoned: %w", err)
	}
	return nil
}
