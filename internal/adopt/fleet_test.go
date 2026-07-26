package adopt

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fastOptions keeps the polling intervals short so these tests run in
// milliseconds; the live defaults are tuned for a real controller.
func fastOptions() Options {
	return Options{PollInterval: 10 * time.Millisecond, RetryInterval: 10 * time.Millisecond}
}

// loginTo builds a client for f and logs it in, failing the test if it cannot.
func loginTo(t *testing.T, url string) *Client {
	t.Helper()
	c := New(url, Classic)
	if err := c.Login(context.Background(), "admin", "admin"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	return c
}

func TestFleetAdoptsEveryMAC(t *testing.T) {
	const mac1, mac2 = "00:15:6d:00:00:01", "00:15:6d:00:00:02"
	f := newFakeClassic(t, mac1, mac2)
	c := loginTo(t, f.server.URL)

	var connected []string
	opts := fastOptions()
	opts.OnConnected = func(d Device) { connected = append(connected, d.MAC) }

	if err := Fleet(waitCtx(t, 5*time.Second), c, "default", []string{mac1, mac2}, opts); err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if want := []string{mac1, mac2}; !equalStrings(connected, want) {
		t.Errorf("OnConnected saw %v, want %v", connected, want)
	}
	if saw := f.sawAdopts(); !equalStrings(saw, []string{mac1, mac2}) {
		t.Errorf("controller saw adopts %v, want one per MAC in order", saw)
	}
}

// TestFleetReportsPendingBeforeAdopting proves the pending document is
// observed first: adopting a device the controller has not yet seen is the
// mistake this wait exists to prevent.
func TestFleetReportsPendingBeforeAdopting(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	c := loginTo(t, f.server.URL)

	var pending []Device
	opts := fastOptions()
	opts.OnPending = func(d Device) { pending = append(pending, d) }

	if err := Fleet(waitCtx(t, 5*time.Second), c, "default", []string{mac}, opts); err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("OnPending called %d times, want 1", len(pending))
	}
	if pending[0].State != 2 || pending[0].Adopted {
		t.Errorf("pending device = %+v, want state 2 not adopted", pending[0])
	}
}

// TestFleetRetriesCannotAdopt covers the controller rejecting an adopt for a
// document that is only seconds old — a routine response, not a failure.
func TestFleetRetriesCannotAdopt(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	f.adoptErr = "api.err.CannotAdopt"
	f.failN = 2
	c := loginTo(t, f.server.URL)

	if err := Fleet(waitCtx(t, 5*time.Second), c, "default", []string{mac}, fastOptions()); err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if saw := f.sawAdopts(); len(saw) != 3 {
		t.Errorf("controller saw %d adopts, want 3 (two rejected, one accepted)", len(saw))
	}
}

// TestFleetRetriesThroughTheNetworkAppPlaceholder is the cold start that
// outlives login. On ucore the login lands against the OS while the Network
// App behind /proxy/network is still starting, so an authenticated call can
// still meet the boot placeholder. It arrives as an HTML body under HTTP 200,
// which is neither a rejection nor CannotAdopt, so a narrow retry loop calls
// it fatal and fails an adoption that had not started yet.
func TestFleetRetriesThroughTheNetworkAppPlaceholder(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	f.placeholderAdopts = 2
	c := loginTo(t, f.server.URL)

	if err := Fleet(waitCtx(t, 5*time.Second), c, "default", []string{mac}, fastOptions()); err != nil {
		t.Fatalf("Fleet through a starting Network App: %v", err)
	}
	if saw := f.sawAdopts(); len(saw) != 3 {
		t.Errorf("controller saw %d adopts, want 3 (two placeholders, one accepted)", len(saw))
	}
}

// TestDeviceByMACReportsTheBootPlaceholder keeps the poll loops' timeout
// message honest. They already tolerate a failing read, so the placeholder
// costs nothing there — but the last error is what the timeout reports, and
// "invalid character '<'" reads like a broken client rather than a
// controller that never came up.
func TestDeviceByMACReportsTheBootPlaceholder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writePlaceholder(w)
	}))
	t.Cleanup(srv.Close)

	c := newClient(srv.URL, Classic, time.Millisecond, 2*time.Millisecond)
	_, err := c.DeviceByMAC(waitCtx(t, 5*time.Second), "default", "00:15:6d:00:00:01")
	if err == nil {
		t.Fatal("DeviceByMAC against a booting controller: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "not the JSON a running controller answers with") {
		t.Errorf("error %q does not say the controller is still starting", err)
	}
}

// TestFleetFailsOnNonRetryableAdoptError checks the retry loop is narrow: an
// error that is not CannotAdopt means the adopt will never work, so retrying
// it would just burn the timeout and bury the real reason.
func TestFleetFailsOnNonRetryableAdoptError(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	f.adoptErr = "api.err.InvalidTarget"
	f.failN = 99
	c := loginTo(t, f.server.URL)

	err := Fleet(waitCtx(t, 5*time.Second), c, "default", []string{mac}, fastOptions())
	if err == nil {
		t.Fatal("Fleet with a rejected adopt: want error, got nil")
	}
	if !strings.Contains(err.Error(), "api.err.InvalidTarget") {
		t.Errorf("error %q does not carry the controller's reason", err)
	}
	if saw := f.sawAdopts(); len(saw) != 1 {
		t.Errorf("controller saw %d adopts, want 1 (no retry on a fatal reason)", len(saw))
	}
}

// TestFleetFailsWhenDeviceNeverPending is the signature of an emulator that
// produced no informs. It must be unmistakable, never mistaken for a slow
// adopt.
func TestFleetFailsWhenDeviceNeverPending(t *testing.T) {
	const listed, missing = "00:15:6d:00:00:01", "00:15:6d:00:00:99"
	f := newFakeClassic(t, listed)
	c := loginTo(t, f.server.URL)

	err := Fleet(waitCtx(t, 300*time.Millisecond), c, "default", []string{missing}, fastOptions())
	if err == nil {
		t.Fatal("Fleet for an unlisted device: want error, got nil")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the device", err)
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Errorf("error %q does not say it never reached pending", err)
	}
	if saw := f.sawAdopts(); len(saw) != 0 {
		t.Errorf("controller saw adopts %v, want none for a device that never appeared", saw)
	}
}

// TestFleetRequirePendingRejectsAlreadyAdopted keeps the live suite's guard:
// on a controller it just booted, an already-adopted device means the
// fixture is dirty, not that there is nothing to do.
func TestFleetRequirePendingRejectsAlreadyAdopted(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	f.markAdopted(mac)
	c := loginTo(t, f.server.URL)

	opts := fastOptions()
	opts.RequirePending = true
	err := Fleet(waitCtx(t, 5*time.Second), c, "default", []string{mac}, opts)
	if err == nil {
		t.Fatal("Fleet with RequirePending on an adopted device: want error, got nil")
	}
	if !strings.Contains(err.Error(), "already adopted") {
		t.Errorf("error %q does not say the device was already adopted", err)
	}
}

// TestFleetAcceptsAlreadyAdopted is the container's case: a restart against a
// controller that still holds the fleet must succeed, not re-adopt.
func TestFleetAcceptsAlreadyAdopted(t *testing.T) {
	const mac = "00:15:6d:00:00:01"
	f := newFakeClassic(t, mac)
	f.markAdopted(mac)
	c := loginTo(t, f.server.URL)

	var connected []string
	opts := fastOptions()
	opts.OnConnected = func(d Device) { connected = append(connected, d.MAC) }

	if err := Fleet(waitCtx(t, 5*time.Second), c, "default", []string{mac}, opts); err != nil {
		t.Fatalf("Fleet on an already-adopted device: %v", err)
	}
	if !equalStrings(connected, []string{mac}) {
		t.Errorf("OnConnected saw %v, want [%s]", connected, mac)
	}
	if saw := f.sawAdopts(); len(saw) != 0 {
		t.Errorf("controller saw adopts %v, want none for an adopted device", saw)
	}
}

// TestFleetAbortsWhenCheckFails covers the harness hook: if the emulator
// container has died, waiting out the timeout tells the operator nothing.
func TestFleetAbortsWhenCheckFails(t *testing.T) {
	const mac = "00:15:6d:00:00:99" // never listed, so the wait loop runs
	f := newFakeClassic(t, "00:15:6d:00:00:01")
	c := loginTo(t, f.server.URL)

	boom := errors.New("emulator container exited")
	opts := fastOptions()
	opts.Check = func() error { return boom }

	err := Fleet(waitCtx(t, 5*time.Second), c, "default", []string{mac}, opts)
	if !errors.Is(err, boom) {
		t.Fatalf("Fleet with a failing Check = %v, want it to wrap %v", err, boom)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
