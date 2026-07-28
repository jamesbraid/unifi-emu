package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A runtime image is handed exactly one device and must run that one, so
// the happy path is a single-element array carrying all four identities.
func TestDecodeDevice(t *testing.T) {
	raw := `[{"model":"U7PRO","mac":"02:00:00:00:00:01","serial":"EMU000000001","name":"fixture-1"}]`
	got, err := decodeDevice(raw)
	if err != nil {
		t.Fatalf("decodeDevice: %v", err)
	}
	want := device{Model: "U7PRO", MAC: "02:00:00:00:00:01", Serial: "EMU000000001", Name: "fixture-1"}
	if got != want {
		t.Errorf("decodeDevice = %+v, want %+v", got, want)
	}
}

// Every rejection here must kill the container rather than start a device
// the herder did not ask for: a fixture that guesses is worse than one that
// dies, because a guess adopts under the wrong identity.
func TestDecodeDeviceRejects(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"missing env", "", "not set"},
		{"blank env", "   ", "not set"},
		{"malformed json", `[{"model":`, "parse"},
		{"not an array", `{"model":"U7PRO"}`, "parse"},
		{"empty array", `[]`, "exactly one device, got 0"},
		{"two devices", `[{"model":"A"},{"model":"B"}]`, "exactly one device, got 2"},
		{"unknown field", `[{"model":"A","adopt_url":"http://x/"}]`, "adopt_url"},
		{"trailing data", `[{"model":"A"}] [{"model":"B"}]`, "trailing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeDevice(tt.raw)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("decodeDevice(%q) error = %v, want substring %q", tt.raw, err, tt.want)
			}
		})
	}
}

// The reported IP has to be the address the controller can reach the
// container on, so loopback, IPv6 and the 169.254/16 address Docker leaves
// behind when no network is attached are all wrong answers.
func TestContainerIPv4(t *testing.T) {
	tests := []struct {
		name  string
		addrs []net.Addr
		want  string
	}{
		{"single endpoint", ipnets("172.18.0.4/16"), "172.18.0.4"},
		{"skips loopback", ipnets("127.0.0.1/8", "172.18.0.4/16"), "172.18.0.4"},
		{"skips link-local", ipnets("169.254.11.2/16", "172.18.0.4/16"), "172.18.0.4"},
		{"skips ipv6", ipnets("::1/128", "fe80::1/64", "fd00::2/64", "172.18.0.4/16"), "172.18.0.4"},
		{"first wins", ipnets("172.18.0.4/16", "10.9.0.7/24"), "172.18.0.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := containerIPv4(lister(tt.addrs, nil))
			if err != nil {
				t.Fatalf("containerIPv4: %v", err)
			}
			if got != tt.want {
				t.Errorf("containerIPv4 = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContainerIPv4Errors(t *testing.T) {
	enumErr := errors.New("no such device")
	tests := []struct {
		name string
		list func() ([]net.Addr, error)
		want string
	}{
		{"no usable address", lister(ipnets("127.0.0.1/8", "169.254.11.2/16", "fd00::2/64"), nil), "no non-loopback IPv4"},
		{"nothing at all", lister(nil, nil), "no non-loopback IPv4"},
		{"enumeration failed", lister(nil, enumErr), "no such device"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := containerIPv4(tt.list)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("containerIPv4 error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

// The inform body is the whole point of the runtime: it ties the herder's
// identities to the endpoint IP only the container itself can discover.
func TestInformBody(t *testing.T) {
	d := device{Model: "U7PRO", MAC: "02:00:00:00:00:01", Serial: "EMU000000001", Name: "fixture-1"}
	b, err := informBody(d, "172.18.0.4", "http://172.18.0.1:8080/inform")
	if err != nil {
		t.Fatalf("informBody: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("informBody produced invalid JSON: %v", err)
	}
	want := map[string]any{
		"model":      "U7PRO",
		"mac":        "02:00:00:00:00:01",
		"serial":     "EMU000000001",
		"name":       "fixture-1",
		"ip":         "172.18.0.4",
		"inform_url": "http://172.18.0.1:8080/inform",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("informBody[%q] = %v, want %v", k, got[k], v)
		}
	}
}

// A controller that is down, slow or not yet listening is normal and must
// not flip the runtime unhealthy: health tracks the runtime process, not
// the controller. So the loop logs and keeps ticking, and only ctx
// cancellation (SIGTERM) stops it.
func TestInformLoopSurvivesFailedPosts(t *testing.T) {
	var mu sync.Mutex
	calls, logged := 0, 0
	post := func(context.Context) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return errors.New("connection refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		informLoop(ctx, time.Millisecond, post, func(string, ...any) {
			mu.Lock()
			logged++
			mu.Unlock()
		})
		close(done)
	}()
	// Let a few ticks fail, then prove cancellation still stops it fast.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("informLoop did not return after cancellation")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Errorf("post called %d times, want the loop to keep retrying after failures", calls)
	}
	if logged != calls {
		t.Errorf("logged %d failures for %d failed posts", logged, calls)
	}
}

// The loop informs immediately rather than sleeping out the first interval,
// so a container is useful the moment it reports ready.
func TestInformLoopPostsBeforeFirstTick(t *testing.T) {
	posted := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go informLoop(ctx, time.Hour, func(context.Context) error {
		select {
		case posted <- struct{}{}:
		default:
		}
		return nil
	}, func(string, ...any) {})
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("informLoop did not post before the first tick")
	}
}

// HEALTHCHECK runs the same binary in probe mode: healthy means the marker
// written after validation exists and the process that wrote it is alive.
// A stale marker left by a dead process must read as unhealthy.
func TestProbe(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ready")
	if err := writeMarker(marker, 4242); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}
	if err := probe(marker, func(pid int) bool { return pid == 4242 }); err != nil {
		t.Errorf("probe with live process: %v", err)
	}
	if err := probe(marker, func(int) bool { return false }); err == nil {
		t.Error("probe with dead process = nil, want error")
	}
	if err := probe(filepath.Join(dir, "absent"), func(int) bool { return true }); err == nil {
		t.Error("probe with no marker = nil, want error")
	}
	if err := os.WriteFile(marker, []byte("not-a-pid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := probe(marker, func(int) bool { return true }); err == nil {
		t.Error("probe with corrupt marker = nil, want error")
	}
}

// The runtime cannot invent an inform target, so a missing or unusable
// URL is fatal at startup rather than a silent no-op loop.
func TestInformURL(t *testing.T) {
	const ok = "http://172.18.0.1:8080/inform"
	if got, err := informURL(ok); err != nil || got != ok {
		t.Errorf("informURL(%q) = %q, %v", ok, got, err)
	}
	for _, raw := range []string{"", "   ", "172.18.0.1:8080/inform", "ftp://host/inform", "http:///inform"} {
		if _, err := informURL(raw); err == nil {
			t.Errorf("informURL(%q) = nil error, want rejection", raw)
		}
	}
}

// The contract is two variables and nothing else. A third one is drift
// worth seeing in the logs, but not worth killing a container over.
func TestWarnExtraEnv(t *testing.T) {
	var lines []string
	warnExtraEnv([]string{
		"PATH=/bin",
		"UNIFI_EMU_INFORM_URL=http://172.18.0.1:8080/inform",
		"UNIFI_EMU_DEVICES_JSON=[]",
		"UNIFI_EMU_ADOPT_URL=https://172.18.0.1:8443",
	}, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	if len(lines) != 1 || !strings.Contains(lines[0], "UNIFI_EMU_ADOPT_URL") {
		t.Errorf("warnExtraEnv logged %q, want one line naming UNIFI_EMU_ADOPT_URL", lines)
	}
}

func ipnets(cidrs ...string) []net.Addr {
	addrs := make([]net.Addr, 0, len(cidrs))
	for _, c := range cidrs {
		ip, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		addrs = append(addrs, &net.IPNet{IP: ip, Mask: n.Mask})
	}
	return addrs
}

func lister(addrs []net.Addr, err error) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) { return addrs, err }
}
