package emu

import (
	"log"
	"os"
	"strings"
	"testing"
)

const adoptKey = "aa03d5cfcc0d08eaf66e1cb798b07522"

// The device-level protocol behaviour (key rotation, adoption gating, the
// state machine) is exercised in the inform package. These two cases cover
// only what the device adds on top: translating the session's Apply effects
// into DeviceState and the mgmt_cfg dedup that keeps repeats out of the log.
func TestApplyResponseDrivesDeviceState(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	d.applyResponse([]byte(`{"_type":"cmd","cmd":"set-adopt","key":"` + adoptKey + `"}`))
	if d.state != StateAdopting {
		t.Errorf("state = %v, want ADOPTING after set-adopt", d.state)
	}
	d.applyResponse([]byte(`{"_type":"cmd","cmd":"setdefault"}`))
	if d.state != StatePending {
		t.Errorf("state = %v, want PENDING after setdefault", d.state)
	}
}

// mgmt_cfg can arrive on every inform, so the device logs it only when its
// content changes: first sight and every change, never an identical repeat.
func TestSetparamLogsMgmtCfgTransitions(t *testing.T) {
	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	cfg := `{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=4c36cd132e0a811601a3e0ca5793b677\n"}`
	d.applyResponse([]byte(cfg))
	d.applyResponse([]byte(cfg)) // identical repeat: must stay silent
	if n := strings.Count(logs.String(), ": mgmt_cfg: "); n != 1 {
		t.Fatalf("identical mgmt_cfg logged %d times, want 1: %q", n, logs.String())
	}
	d.applyResponse([]byte(`{"_type":"setparam","mgmt_cfg":"cfgversion=def456\nauthkey=4c36cd132e0a811601a3e0ca5793b677\n"}`))
	if n := strings.Count(logs.String(), ": mgmt_cfg: "); n != 2 {
		t.Fatalf("changed mgmt_cfg logged %d times total, want 2: %q", n, logs.String())
	}
}

// The controller resends the same mgmt_cfg on every inform until the device
// acknowledges provisioning. Once the device has reached CONNECTED, repeats
// must be inert: re-rotating the authkey would flap the state back to
// ADOPTING (the classic stuck-adopt-loop bug).
func TestConnectedDeviceIgnoresRepeatedMgmtCfg(t *testing.T) {
	d := mustDevice(t, DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"})
	cfg := `{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=` + adoptKey + `\n"}`
	d.applyResponse([]byte(cfg))
	if d.state != StateAdopting {
		t.Fatalf("state = %v, want ADOPTING after mgmt_cfg authkey rotation", d.state)
	}
	if key := d.session.AuthKey(); key != adoptKey {
		t.Fatalf("AuthKey() = %q, want %q", key, adoptKey)
	}

	// The inform loop is what promotes ADOPTING to CONNECTED in production;
	// set it directly here since this test drives applyResponse on its own.
	d.state = StateConnected

	d.applyResponse([]byte(cfg))
	d.applyResponse([]byte(cfg))
	if d.state != StateConnected {
		t.Errorf("state = %v, want CONNECTED undisturbed by mgmt_cfg repeats", d.state)
	}
	if key := d.session.AuthKey(); key != adoptKey {
		t.Errorf("AuthKey() = %q, want unchanged %q", key, adoptKey)
	}
}
