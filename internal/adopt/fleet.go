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
)

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

// Fleet drives every MAC in macs to a connected, adopted device on the
// controller: wait for the pending document, issue the adopt, wait for
// state 1.
//
// Devices go one at a time and the first failure stops the run. That is not
// caution — controllers reject bursts of adopts, and reject documents that
// are only seconds old, so adopting in parallel is slower and flakier than
// adopting in sequence.
//
// The whole run is bounded by ctx. Each phase reports its own failure, so a
// timeout says whether the device never informed, never adopted, or never
// finished provisioning.
func Fleet(ctx context.Context, c *Client, site string, macs []string, opts Options) error {
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
		device, err = c.WaitAdopted(ctx, site, mac)
		if err != nil {
			return fmt.Errorf("%s controller-side adoption: %w", mac, err)
		}
		if opts.OnConnected != nil {
			opts.OnConnected(device)
		}
	}
	return nil
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
