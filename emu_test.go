package emu

import (
	"context"
	"strings"
	"testing"
	"time"
)

func waitCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestAddValidatesMAC(t *testing.T) {
	e := New("http://unifi:8080/inform")
	if err := e.Add(DeviceSpec{MAC: "not-a-mac", Model: "U7MP"}); err == nil {
		t.Error("Add with unparseable MAC: want error, got nil")
	}
	if err := e.Add(DeviceSpec{MAC: "00:15:6d:ff:fe:00:00:01", Model: "U7MP"}); err == nil {
		t.Error("Add with EUI-64 address: want six-byte MAC error, got nil")
	}
	good := DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"}
	if err := e.Add(good); err != nil {
		t.Fatalf("Add valid spec: %v", err)
	}
	if err := e.Add(good); err == nil {
		t.Error("Add duplicate MAC: want error, got nil")
	}
	upper := good
	upper.MAC = "00:15:6D:00:00:01"
	if err := e.Add(upper); err == nil {
		t.Error("Add duplicate MAC spelled in upper case: want error, got nil")
	}
	if err := e.Add(DeviceSpec{MAC: "00:15:6d:00:00:02", Model: "NOPE"}); err == nil {
		t.Error("Add unknown model: want error, got nil")
	}
}

func TestStartRejectsNonPositiveInformInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		e := New("http://unifi:8080/inform", WithInformInterval(interval))
		if err := e.Add(uapSpec()); err != nil {
			t.Fatalf("Add with interval %v: %v", interval, err)
		}
		if err := e.Start(context.Background()); err == nil {
			t.Errorf("Start with interval %v: want error, got nil", interval)
		} else if !strings.Contains(err.Error(), "inform interval") {
			t.Errorf("Start with interval %v error %q does not name interval", interval, err)
		}
	}
}

func TestStateUnknownMAC(t *testing.T) {
	e := New("http://unifi:8080/inform")
	if err := e.Add(uapSpec()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, ok := e.State("00:15:6d:00:00:99"); ok {
		t.Error("State of unknown MAC: ok = true, want false")
	}
	if _, ok := e.State("garbage"); ok {
		t.Error("State of unparseable MAC: ok = true, want false")
	}
	if got, ok := e.State(strings.ToUpper(uapSpec().MAC)); !ok || got != StatePending {
		t.Errorf("State of added MAC queried in upper case = %v, %v; want PENDING, true", got, ok)
	}
}

func TestStartEmptyFleetErrors(t *testing.T) {
	e := New("http://unifi:8080/inform")
	if err := e.Start(context.Background()); err == nil {
		t.Fatal("Start with no devices: want error, got nil")
	} else if !strings.Contains(err.Error(), "no devices") {
		t.Errorf("Start error %q does not mention missing devices", err)
	}
	// A rejected Start must not weld the fleet shut.
	if err := e.Add(uapSpec()); err != nil {
		t.Fatalf("Add after rejected Start: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start after Add: %v", err)
	}
	defer e.Stop()
}

func TestAddAfterStartRejected(t *testing.T) {
	e := New("http://unifi:8080/inform")
	if err := e.Add(uapSpec()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()
	if err := e.Add(uswSpec()); err == nil {
		t.Error("Add after Start: want error, got nil")
	} else if !strings.Contains(err.Error(), "after Start") {
		t.Errorf("Add after Start error %q does not mention Start", err)
	}
	// Start is one-shot, so Add stays rejected even after Stop.
	e.Stop()
	if err := e.Add(uswSpec()); err == nil {
		t.Error("Add after Stop: want error, got nil")
	}
}

func TestStartTwiceErrors(t *testing.T) {
	e := New("http://unifi:8080/inform")
	if err := e.Add(uapSpec()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer e.Stop()
	if err := e.Start(context.Background()); err == nil {
		t.Error("second Start: want error, got nil")
	}
}

func TestWaitState(t *testing.T) {
	fc := newFakeController(t, nil)
	e := New(fc.informURL(), WithInformInterval(20*time.Millisecond))
	specs := []DeviceSpec{ugwSpec(), uswSpec(), uapSpec()}
	if err := e.Add(specs...); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer e.Stop()

	for _, spec := range specs {
		if err := e.WaitState(waitCtx(t, 2*time.Second), spec.MAC, StatePending); err != nil {
			t.Errorf("%s pending: %v", spec.Model, err)
		}
	}

	fc.adoptCalled.Store(true)
	for _, spec := range specs {
		if err := e.WaitState(waitCtx(t, 2*time.Second), spec.MAC, StateConnected); err != nil {
			t.Errorf("%s connected: %v", spec.Model, err)
		}
	}

	// An unreachable state must time out and name the device's last state.
	err := e.WaitState(waitCtx(t, 100*time.Millisecond), specs[0].MAC, DeviceState(42))
	if err == nil {
		t.Fatal("WaitState for impossible state: want error, got nil")
	}
	if !strings.Contains(err.Error(), StateConnected.String()) {
		t.Errorf("timeout error %q does not name the last state %q", err, StateConnected)
	}
	if err := e.WaitState(waitCtx(t, time.Second), "00:15:6d:00:00:99", StatePending); err == nil {
		t.Error("WaitState for unknown MAC: want error, got nil")
	}

	// Stop must be safe to call again (the deferred Stop above runs third).
	e.Stop()
}
