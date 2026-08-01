package herder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// --- fake backend -----------------------------------------------------------

type fakeInstance struct {
	id            string
	unit          Unit
	terminated    bool
	terminateErr  error
	terminateWith time.Duration
	terminateHold time.Duration
	logs          string
	mu            sync.Mutex
}

func (f *fakeInstance) ID() string { return f.id }

func (f *fakeInstance) Terminate(ctx context.Context, stopTimeout time.Duration) error {
	if f.terminateHold > 0 {
		select {
		case <-time.After(f.terminateHold):
		case <-ctx.Done():
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = true
	f.terminateWith = stopTimeout
	return f.terminateErr
}

func (f *fakeInstance) Logs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.logs)), nil
}

func (f *fakeInstance) wasTerminated() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.terminated
}

type fakeBackend struct {
	mu sync.Mutex

	daemonErr  error
	networkErr error
	capErr     error
	prepareErr error

	// launchErrs and waitErrs are keyed by the unit's request-index set,
	// not by call order: units start in parallel, so an ordinal key would
	// name a different unit on every run.
	launchErrs   map[string]error
	waitErrs     map[string]error
	launched     []Unit
	instances    []*fakeInstance
	terminateErr error

	// states answers Inspect. stateFn wins when set; it sees the poll count
	// so a test can change behaviour after ready.
	states  map[string]ContainerState
	stateFn func(id string, poll int) (ContainerState, error)
	polls   int

	remaining    int
	remainingErr error
}

func (b *fakeBackend) CheckDaemon(context.Context) error { return b.daemonErr }

func (b *fakeBackend) CheckNetwork(context.Context, string) error { return b.networkErr }

func (b *fakeBackend) CheckCapabilities(context.Context, Plan) error { return b.capErr }

func (b *fakeBackend) Prepare(context.Context, Plan) error { return b.prepareErr }

func (b *fakeBackend) Launch(_ context.Context, _ Plan, unit Unit) (Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.launched = append(b.launched, unit)
	key := unit.DeviceIndices()
	if err, ok := b.launchErrs[key]; ok {
		return nil, err
	}
	inst := &fakeInstance{id: "c" + key, unit: unit, terminateErr: b.terminateErr}
	b.instances = append(b.instances, inst)
	return inst, nil
}

func (b *fakeBackend) WaitHealthy(_ context.Context, inst Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, candidate := range b.instances {
		if candidate.id == inst.ID() {
			return b.waitErrs[candidate.unit.DeviceIndices()]
		}
	}
	return nil
}

func (b *fakeBackend) Inspect(_ context.Context, id string) (ContainerState, error) {
	b.mu.Lock()
	poll := b.polls
	b.polls++
	fn := b.stateFn
	b.mu.Unlock()
	if fn != nil {
		return fn(id, poll)
	}
	if state, ok := b.states[id]; ok {
		return state, nil
	}
	return b.healthy(id, "172.28.0.4"), nil
}

// healthy is what Docker reports for a container the herder just started:
// running, healthy, on the caller network, and carrying the endpoint MAC the
// request assigned. A fake that omitted the MAC would fail every standalone
// unit's endpoint check for the wrong reason.
func (b *fakeBackend) healthy(id, ip string) ContainerState {
	state := healthyOn("net", ip)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, inst := range b.instances {
		if inst.id == id {
			state.Networks["net"] = Endpoint{IPv4: ip, MAC: inst.unit.EndpointMAC()}
		}
	}
	return state
}

func (b *fakeBackend) Remaining(context.Context, string) (int, error) {
	return b.remaining, b.remainingErr
}

func (b *fakeBackend) launchCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.launched)
}

func healthyOn(networkName, ip string) ContainerState {
	return ContainerState{
		Running: true, Health: "healthy", StartedAt: "t0",
		Networks: map[string]Endpoint{networkName: {IPv4: ip}},
	}
}

// --- harness ----------------------------------------------------------------

type runResult struct {
	exit   int
	events []map[string]any
	stderr string
}

func (r runResult) terminal(t *testing.T) map[string]any {
	t.Helper()
	for i := len(r.events) - 1; i >= 0; i-- {
		if ev := r.events[i]["event"]; ev == "failed" || ev == "stopped" {
			return r.events[i]
		}
	}
	t.Fatalf("no terminal event in %#v", r.events)
	return nil
}

func (r runResult) event(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, ev := range r.events {
		if ev["event"] == name {
			return ev
		}
	}
	t.Fatalf("no %s event in %#v", name, r.events)
	return nil
}

func (r runResult) has(name string) bool {
	for _, ev := range r.events {
		if ev["event"] == name {
			return true
		}
	}
	return false
}

type runHarness struct {
	request string
	config  string
	backend *fakeBackend
	signals chan os.Signal
	stdout  io.Writer
	reaper  error
	synth   string
	network string
	inform  string
	poll    time.Duration
	startup time.Duration
	stop    time.Duration
}

func newHarness(t *testing.T) *runHarness {
	t.Helper()
	return &runHarness{
		request: `{"version":1,"devices":[{"model":"USM8P"}]}`,
		backend: &fakeBackend{launchErrs: map[string]error{}, waitErrs: map[string]error{}},
		signals: make(chan os.Signal, 4),
		synth:   "public/emu:1.0.0",
		network: "net",
		inform:  "http://172.28.0.2:8080/inform",
		poll:    time.Millisecond,
		startup: 5 * time.Second,
		stop:    30 * time.Second,
	}
}

func (h *runHarness) run(t *testing.T) runResult {
	t.Helper()
	return h.runCtx(t, context.Background())
}

func (h *runHarness) runCtx(t *testing.T, ctx context.Context) runResult {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	out := io.Writer(&stdout)
	if h.stdout != nil {
		out = h.stdout
	}
	configPath := ""
	if h.config != "" {
		configPath = writeConfig(t, h.config)
	}
	newBackend := func(context.Context) (Backend, error) {
		if h.backend == nil {
			return nil, failf(CodeDockerUnavailable, PhaseValidate, "cannot connect to the Docker daemon")
		}
		return h.backend, nil
	}
	var backend Backend
	if h.backend != nil {
		backend = h.backend
	}
	exit := Run(ctx, Options{
		Network:           h.network,
		InformURL:         h.inform,
		Request:           strings.NewReader(h.request),
		RuntimeConfigPath: configPath,
		SyntheticImage:    h.synth,
		StartupTimeout:    h.startup,
		StopTimeout:       h.stop,
		PollInterval:      h.poll,
		Stdout:            out,
		Stderr:            &stderr,
		Getenv:            func(string) string { return "" },
		RunID:             "run1",
		Backend:           backend,
		NewBackend:        newBackend,
		CheckReaper:       func() error { return h.reaper },
		Signals:           h.signals,
	})
	body := stdout.String()
	if s, ok := out.(fmt.Stringer); ok {
		body = s.String()
	}
	return runResult{exit: exit, events: decodeEvents(t, body), stderr: stderr.String()}
}

func decodeEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout line %q is not one JSON object: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// signalAfterReady sends sig the moment the ready event reaches stdout.
// Keying off the protocol rather than off a poll count is what makes the
// stop happen in the running phase for every fleet size.
func (h *runHarness) signalAfterReady(sig os.Signal) {
	h.stdout = &signalOnReady{signals: h.signals, sig: sig}
}

type signalOnReady struct {
	buf     bytes.Buffer
	signals chan os.Signal
	sig     os.Signal
	sent    bool
}

func (w *signalOnReady) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if !w.sent && bytes.Contains(p, []byte(`"event":"ready"`)) {
		w.sent = true
		w.signals <- w.sig
	}
	return n, err
}

func (w *signalOnReady) String() string { return w.buf.String() }

// --- protocol shape ---------------------------------------------------------

func TestRunEmitsStartedFirstEvenWhenValidationFails(t *testing.T) {
	h := newHarness(t)
	h.request = `{"version":9,"devices":[]}`
	got := h.run(t)
	if len(got.events) == 0 || got.events[0]["event"] != "started" {
		t.Fatalf("events = %#v, want started first", got.events)
	}
	if got.events[0]["run_id"] != "run1" {
		t.Fatalf("started run_id = %v", got.events[0]["run_id"])
	}
}

func TestRunHappyPathEmitsStartedReadyStopped(t *testing.T) {
	h := newHarness(t)
	h.signalAfterReady(syscall.SIGTERM)
	got := h.run(t)
	if got.exit != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", got.exit, got.stderr)
	}
	if len(got.events) != 3 {
		t.Fatalf("events = %#v, want started, ready and stopped", got.events)
	}
	if got.events[0]["event"] != "started" || got.events[1]["event"] != "ready" ||
		got.events[2]["event"] != "stopped" {
		t.Fatalf("event order = %#v", got.events)
	}
	if got.events[2]["reason"] != "signal" {
		t.Fatalf("stopped reason = %v, want signal", got.events[2]["reason"])
	}
}

func TestReadyCarriesInspectedIPsPerDeviceInRequestOrder(t *testing.T) {
	h := newHarness(t)
	h.request = `{"version":1,"devices":[{"model":"USM8P"},{"model":"UGW3"},{"model":"UXGENT"}]}`
	h.config = `{"version":1,"models":{"UXGENT":{"image":"fixture.example/x@` + testDigest + `"}}}`
	h.backend.stateFn = func(id string, _ int) (ContainerState, error) {
		if id == "c0,1" {
			return h.backend.healthy(id, "172.28.0.4"), nil
		}
		return h.backend.healthy(id, "172.28.0.5"), nil
	}
	h.signalAfterReady(syscall.SIGINT)
	got := h.run(t)
	if got.exit != 0 {
		t.Fatalf("exit = %d (%s)", got.exit, got.stderr)
	}
	ready := got.event(t, "ready")
	devices, _ := ready["devices"].([]any)
	if len(devices) != 3 {
		t.Fatalf("ready devices = %#v, want 3", ready["devices"])
	}
	for i, raw := range devices {
		d := raw.(map[string]any)
		if int(d["index"].(float64)) != i {
			t.Fatalf("device %d has index %v", i, d["index"])
		}
		if d["ip"] == "" || d["ip"] == nil {
			t.Fatalf("device %d has no ip: %#v", i, d)
		}
	}
	// The two batched synthetic devices intentionally share the batch
	// container's address; the standalone runtime has its own.
	if devices[0].(map[string]any)["ip"] != devices[1].(map[string]any)["ip"] {
		t.Fatal("batched synthetic devices report different IPs")
	}
	if devices[2].(map[string]any)["ip"] == devices[0].(map[string]any)["ip"] {
		t.Fatal("the standalone runtime shares the synthetic batch IP")
	}
}

func TestOnlyTheReadyEventCarriesDeviceIPs(t *testing.T) {
	h := newHarness(t)
	h.backend.waitErrs["0"] = errors.New("health never arrived")
	got := h.run(t)
	for _, ev := range got.events {
		if ev["event"] == "ready" {
			continue
		}
		body, _ := json.Marshal(ev)
		if strings.Contains(string(body), `"ip"`) {
			t.Fatalf("%v event carries a device ip: %s", ev["event"], body)
		}
	}
}

// --- validation: nothing is created ----------------------------------------

func TestValidationFailuresCreateNoContainer(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*runHarness)
		code  Code
	}{
		{"invalid request", func(h *runHarness) {
			h.request = `{"version":1,"devices":[{"model":"USM8P","ip":"1.2.3.4"}]}`
		}, CodeInvalidRequest},
		{"duplicate MAC", func(h *runHarness) {
			h.request = `{"version":1,"devices":[{"model":"USM8P","mac":"02:00:00:00:10:01"},` +
				`{"model":"UGW3","mac":"02:00:00:00:10:01"}]}`
		}, CodeInvalidRequest},
		{"invalid runtime config", func(h *runHarness) {
			h.config = `{"version":1,"models":{"UXGENT":{"image":"forge.example/x:tag"}}}`
		}, CodeInvalidRuntimeConfig},
		{"inform URL", func(h *runHarness) { h.inform = "http://controller:8080/inform" },
			CodeInformURLInvalid},
		{"reaper disabled", func(h *runHarness) {
			h.reaper = failf(CodeReaperDisabled, PhaseValidate, "disabled")
		}, CodeReaperDisabled},
		{"docker unavailable", func(h *runHarness) {
			h.backend.daemonErr = failf(CodeDockerUnavailable, PhaseValidate, "no daemon")
		}, CodeDockerUnavailable},
		{"docker too old", func(h *runHarness) {
			h.backend.daemonErr = failf(CodeDockerVersionUnsupported, PhaseValidate, "24.0")
		}, CodeDockerVersionUnsupported},
		{"network missing", func(h *runHarness) {
			h.backend.networkErr = failf(CodeNetworkNotFound, PhaseValidate, "absent")
		}, CodeNetworkNotFound},
		{"unknown model", func(h *runHarness) {
			h.request = `{"version":1,"devices":[{"model":"NOPE"}]}`
		}, CodeSyntheticModelUnknown},
		{"no synthetic image", func(h *runHarness) { h.synth = "" }, CodeSyntheticImageRequired},
		{"capability unsupported", func(h *runHarness) {
			h.backend.capErr = failf(CodeCapabilityUnsupported, PhaseValidate, "rootless")
		}, CodeCapabilityUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			c.setup(h)
			got := h.run(t)
			if got.exit != 1 {
				t.Fatalf("exit = %d, want 1", got.exit)
			}
			terminal := got.terminal(t)
			if terminal["event"] != "failed" {
				t.Fatalf("terminal event = %v, want failed", terminal["event"])
			}
			if terminal["code"] != string(c.code) {
				t.Fatalf("code = %v, want %s", terminal["code"], c.code)
			}
			if terminal["phase"] != string(PhaseValidate) {
				t.Fatalf("phase = %v, want validate", terminal["phase"])
			}
			if terminal["cleanup_complete"] != true {
				t.Fatalf("cleanup_complete = %v, want true", terminal["cleanup_complete"])
			}
			if n := h.backend.launchCount(); n != 0 {
				t.Fatalf("created %d container(s) after a validation failure", n)
			}
			if got.has("ready") {
				t.Fatal("a validation failure still emitted ready")
			}
		})
	}
}

func TestPullFailureIsReportedInThePullPhase(t *testing.T) {
	h := newHarness(t)
	h.backend.prepareErr = failf(CodeImagePullFailed, PhasePull, "manifest unknown")
	got := h.run(t)
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeImagePullFailed) || terminal["phase"] != string(PhasePull) {
		t.Fatalf("terminal = %#v", terminal)
	}
	if h.backend.launchCount() != 0 {
		t.Fatal("a pull failure still created a container")
	}
}

func TestImageInvalidIsReportedInThePullPhase(t *testing.T) {
	h := newHarness(t)
	h.backend.prepareErr = failf(CodeImageInvalid, PhasePull, "no healthcheck")
	got := h.run(t)
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeImageInvalid) || terminal["phase"] != string(PhasePull) {
		t.Fatalf("terminal = %#v", terminal)
	}
}

// Docker's own preflight runs before any container is created, which is what
// makes "invalid input creates no container" true for image problems too.
func TestPreparationRunsBeforeAnyLaunch(t *testing.T) {
	h := newHarness(t)
	order := []string{}
	h.backend.prepareErr = nil
	h.backend.launchErrs["0"] = errors.New("must not be reached first")
	h.backend.prepareErr = failf(CodeImagePullFailed, PhasePull, "registry down")
	_ = order
	got := h.run(t)
	if h.backend.launchCount() != 0 {
		t.Fatalf("launch happened despite a failed preflight (%#v)", got.terminal(t))
	}
}

// --- topology --------------------------------------------------------------

func TestOneSyntheticContainerHoldsEveryUnmappedDevice(t *testing.T) {
	h := newHarness(t)
	h.request = `{"version":1,"devices":[{"model":"USM8P"},{"model":"UGW3"},{"model":"U7PRO"}]}`
	h.signalAfterReady(syscall.SIGTERM)
	got := h.run(t)
	if got.exit != 0 {
		t.Fatalf("exit = %d (%s)", got.exit, got.stderr)
	}
	if n := h.backend.launchCount(); n != 1 {
		t.Fatalf("created %d containers, want one synthetic batch", n)
	}
	if len(h.backend.launched[0].Devices) != 3 {
		t.Fatalf("synthetic unit holds %d devices", len(h.backend.launched[0].Devices))
	}
}

func TestEachMappedDeviceGetsItsOwnContainer(t *testing.T) {
	h := newHarness(t)
	h.request = `{"version":1,"devices":[{"model":"USM8P"},{"model":"UXGENT"},{"model":"UXGENT"}]}`
	h.config = `{"version":1,"models":{"UXGENT":{"image":"fixture.example/x@` + testDigest + `"}}}`
	h.signalAfterReady(syscall.SIGTERM)
	got := h.run(t)
	if got.exit != 0 {
		t.Fatalf("exit = %d (%s)", got.exit, got.stderr)
	}
	if n := h.backend.launchCount(); n != 3 {
		t.Fatalf("created %d containers, want one synthetic plus two standalone", n)
	}
	opaque := 0
	for _, unit := range h.backend.launched {
		if unit.Kind == UnitOpaque {
			opaque++
			if len(unit.Devices) != 1 {
				t.Fatalf("standalone unit holds %d devices", len(unit.Devices))
			}
		}
	}
	if opaque != 2 {
		t.Fatalf("standalone containers = %d, want 2", opaque)
	}
}

// --- start phase -----------------------------------------------------------

func TestLaunchFailureRollsBackTheContainersAlreadyStarted(t *testing.T) {
	h := newHarness(t)
	h.request = `{"version":1,"devices":[{"model":"USM8P"},{"model":"UXGENT"}]}`
	h.config = `{"version":1,"models":{"UXGENT":{"image":"fixture.example/x@` + testDigest + `"}}}`
	h.backend.launchErrs["1"] = errors.New("no such image")
	got := h.run(t)
	if got.exit != 1 {
		t.Fatalf("exit = %d, want 1", got.exit)
	}
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeContainerStartFailed) || terminal["phase"] != string(PhaseStart) {
		t.Fatalf("terminal = %#v", terminal)
	}
	if terminal["cleanup_complete"] != true {
		t.Fatalf("cleanup_complete = %v, want true", terminal["cleanup_complete"])
	}
	for _, inst := range h.backend.instances {
		if !inst.wasTerminated() {
			t.Fatalf("container %s survived the rollback", inst.id)
		}
	}
	if got.has("ready") {
		t.Fatal("a partial start still emitted ready")
	}
}

func TestEndpointMACMismatchFailsBeforeReady(t *testing.T) {
	h := newHarness(t)
	h.request = `{"version":1,"devices":[{"model":"UXGENT","mac":"02:00:00:00:10:01"}]}`
	h.config = `{"version":1,"models":{"UXGENT":{"image":"fixture.example/x@` + testDigest + `"}}}`
	h.backend.stateFn = func(string, int) (ContainerState, error) {
		state := healthyOn("net", "172.28.0.5")
		state.Networks["net"] = Endpoint{IPv4: "172.28.0.5", MAC: "02:ff:ff:ff:ff:ff"}
		return state, nil
	}
	got := h.run(t)
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeEndpointMACMismatch) || terminal["phase"] != string(PhaseStart) {
		t.Fatalf("terminal = %#v", terminal)
	}
	if got.has("ready") {
		t.Fatal("a MAC mismatch still emitted ready")
	}
}

func TestHostPortBindingFailsTheStartPhase(t *testing.T) {
	h := newHarness(t)
	h.backend.stateFn = func(string, int) (ContainerState, error) {
		state := healthyOn("net", "172.28.0.4")
		state.HostPortBindings = 1
		return state, nil
	}
	got := h.run(t)
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeContainerStartFailed) || terminal["phase"] != string(PhaseStart) {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestExtraNetworkBeforeReadyFailsTheStartPhase(t *testing.T) {
	h := newHarness(t)
	h.backend.stateFn = func(string, int) (ContainerState, error) {
		state := healthyOn("net", "172.28.0.4")
		state.Networks["bridge"] = Endpoint{IPv4: "172.17.0.2"}
		return state, nil
	}
	got := h.run(t)
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeContainerStartFailed) || terminal["phase"] != string(PhaseStart) {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestReadyWaitsForEveryHealthcheck(t *testing.T) {
	h := newHarness(t)
	h.request = `{"version":1,"devices":[{"model":"USM8P"},{"model":"UXGENT"}]}`
	h.config = `{"version":1,"models":{"UXGENT":{"image":"fixture.example/x@` + testDigest + `"}}}`
	h.backend.waitErrs["1"] = errors.New("health check never passed")
	got := h.run(t)
	if got.has("ready") {
		t.Fatal("ready was emitted while one container was still unhealthy")
	}
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeDeviceUnhealthy) || terminal["phase"] != string(PhaseHealth) {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestFailedEventNamesTheDevicesOfTheFailingUnit(t *testing.T) {
	h := newHarness(t)
	h.request = `{"version":1,"devices":[{"model":"USM8P"},{"model":"UXGENT","mac":"02:00:00:00:10:01"}]}`
	h.config = `{"version":1,"models":{"UXGENT":{"image":"fixture.example/x@` + testDigest + `"}}}`
	h.backend.waitErrs["1"] = errors.New("health check never passed")
	got := h.run(t)
	terminal := got.terminal(t)
	devices, _ := terminal["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("failed devices = %#v, want the failing unit's device", terminal["devices"])
	}
	if devices[0].(map[string]any)["mac"] != "02:00:00:00:10:01" {
		t.Fatalf("failed device = %#v", devices[0])
	}
}

// --- running phase ---------------------------------------------------------

func TestPostReadyFailuresFailTheFleet(t *testing.T) {
	cases := []struct {
		name  string
		state func(ContainerState) ContainerState
		code  Code
	}{
		{"exit", func(s ContainerState) ContainerState { s.Running = false; s.ExitCode = 1; return s },
			CodeDeviceExited},
		{"unhealthy", func(s ContainerState) ContainerState { s.Health = "unhealthy"; return s },
			CodeDeviceUnhealthy},
		{"restart count", func(s ContainerState) ContainerState { s.RestartCount = 2; return s },
			CodeDeviceRestarted},
		{"restart timestamp", func(s ContainerState) ContainerState { s.StartedAt = "t1"; return s },
			CodeDeviceRestarted},
		{"network loss", func(s ContainerState) ContainerState {
			return ContainerState{Running: true, Health: "healthy", StartedAt: "t0",
				Networks: map[string]Endpoint{"other": {IPv4: "10.0.0.2"}}}
		}, CodeNetworkDetached},
		{"extra network", func(s ContainerState) ContainerState {
			s.Networks = map[string]Endpoint{
				"net": {IPv4: "172.28.0.4"}, "bridge": {IPv4: "172.17.0.2"}}
			return s
		}, CodeNetworkDetached},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			mutate := c.state
			h.backend.stateFn = func(_ string, poll int) (ContainerState, error) {
				base := healthyOn("net", "172.28.0.4")
				if poll < 2 {
					return base, nil
				}
				return mutate(base), nil
			}
			got := h.run(t)
			if got.exit != 1 {
				t.Fatalf("exit = %d, want 1", got.exit)
			}
			if !got.has("ready") {
				t.Fatalf("the run never reached ready: %#v", got.events)
			}
			terminal := got.terminal(t)
			if terminal["code"] != string(c.code) {
				t.Fatalf("code = %v, want %s", terminal["code"], c.code)
			}
			if terminal["phase"] != string(PhaseRuntime) {
				t.Fatalf("phase = %v, want runtime", terminal["phase"])
			}
			for _, inst := range h.backend.instances {
				if !inst.wasTerminated() {
					t.Fatalf("container %s survived a runtime failure", inst.id)
				}
			}
		})
	}
}

func TestThreeConsecutiveDockerFailuresFailTheFleet(t *testing.T) {
	h := newHarness(t)
	h.backend.stateFn = func(_ string, poll int) (ContainerState, error) {
		if poll < 2 {
			return healthyOn("net", "172.28.0.4"), nil
		}
		return ContainerState{}, errors.New("cannot connect to the Docker daemon")
	}
	got := h.run(t)
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeDockerUnavailable) || terminal["phase"] != string(PhaseRuntime) {
		t.Fatalf("terminal = %#v", terminal)
	}
}

// A single blip is not a lost daemon: the counter resets on the next success,
// so a transient inspect error must not tear down a healthy fleet.
func TestASuccessfulInspectResetsTheDockerFailureCount(t *testing.T) {
	h := newHarness(t)
	var failures int
	h.backend.stateFn = func(_ string, poll int) (ContainerState, error) {
		if poll < 2 {
			return healthyOn("net", "172.28.0.4"), nil
		}
		// Alternate: two failures, one success, forever. That never
		// reaches three in a row, so the run must survive until the signal.
		if poll%3 != 0 {
			failures++
			if failures > 12 {
				h.signals <- syscall.SIGTERM
			}
			return ContainerState{}, errors.New("temporary failure")
		}
		return healthyOn("net", "172.28.0.4"), nil
	}
	got := h.run(t)
	if got.exit != 0 {
		t.Fatalf("exit = %d, want 0: transient inspect errors failed the run (%s)", got.exit, got.stderr)
	}
	if got.terminal(t)["event"] != "stopped" {
		t.Fatalf("terminal = %#v, want stopped", got.terminal(t))
	}
}

// --- cleanup ---------------------------------------------------------------

func TestCleanupFailureTakesPrecedenceAndNamesItsCause(t *testing.T) {
	h := newHarness(t)
	h.backend.remaining = 1
	h.backend.stateFn = func(_ string, poll int) (ContainerState, error) {
		base := healthyOn("net", "172.28.0.4")
		if poll < 2 {
			return base, nil
		}
		base.Health = "unhealthy"
		return base, nil
	}
	got := h.run(t)
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeCleanupFailed) {
		t.Fatalf("code = %v, want cleanup_failed", terminal["code"])
	}
	if terminal["phase"] != string(PhaseCleanup) {
		t.Fatalf("phase = %v, want cleanup", terminal["phase"])
	}
	if terminal["cause"] != string(CodeDeviceUnhealthy) {
		t.Fatalf("cause = %v, want device_unhealthy", terminal["cause"])
	}
	if terminal["cleanup_complete"] != false {
		t.Fatalf("cleanup_complete = %v, want false", terminal["cleanup_complete"])
	}
	if got.exit != 1 {
		t.Fatalf("exit = %d, want 1", got.exit)
	}
}

func TestDockerLossDuringCleanupIsItsOwnCause(t *testing.T) {
	h := newHarness(t)
	h.backend.remainingErr = errors.New("cannot connect to the Docker daemon")
	h.signalAfterReady(syscall.SIGTERM)
	got := h.run(t)
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeCleanupFailed) {
		t.Fatalf("code = %v, want cleanup_failed", terminal["code"])
	}
	if terminal["cause"] != string(CodeDockerUnavailable) {
		t.Fatalf("cause = %v, want docker_unavailable", terminal["cause"])
	}
	if got.exit != 1 {
		t.Fatalf("exit = %d, want 1", got.exit)
	}
}

func TestTerminateFailureIsACleanupFailure(t *testing.T) {
	h := newHarness(t)
	h.backend.terminateErr = errors.New("device or resource busy")
	h.signalAfterReady(syscall.SIGTERM)
	got := h.run(t)
	if got.terminal(t)["code"] != string(CodeCleanupFailed) {
		t.Fatalf("terminal = %#v, want cleanup_failed", got.terminal(t))
	}
}

// Once the herder is stopping, another stop signal must not restart cleanup,
// abort it, or shorten the stop timeout.
func TestRepeatedSignalsDuringStoppingChangeNothing(t *testing.T) {
	h := newHarness(t)
	h.stop = 7 * time.Second
	h.signalAfterReady(syscall.SIGTERM)
	h.backend.launchErrs = map[string]error{}
	got := h.runWithSlowTerminate(t, 60*time.Millisecond, 5)
	if got.exit != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", got.exit, got.stderr)
	}
	if got.terminal(t)["event"] != "stopped" {
		t.Fatalf("terminal = %#v, want stopped", got.terminal(t))
	}
	if n := len(got.events); n != 3 {
		t.Fatalf("events = %#v, want exactly started, ready and stopped", got.events)
	}
	for _, inst := range h.backend.instances {
		if !inst.wasTerminated() {
			t.Fatalf("container %s was not terminated", inst.id)
		}
		if inst.terminateWith != h.stop {
			t.Fatalf("stop timeout = %s, want the unshortened %s", inst.terminateWith, h.stop)
		}
	}
}

// runWithSlowTerminate holds Terminate open for hold and floods extra stop
// signals while it runs.
func (h *runHarness) runWithSlowTerminate(t *testing.T, hold time.Duration, extra int) runResult {
	t.Helper()
	original := h.backend.stateFn
	h.backend.stateFn = func(id string, poll int) (ContainerState, error) {
		h.backend.mu.Lock()
		for _, inst := range h.backend.instances {
			inst.terminateHold = hold
		}
		h.backend.mu.Unlock()
		if original != nil {
			return original(id, poll)
		}
		return h.backend.healthy(id, "172.28.0.4"), nil
	}
	go func() {
		for i := 0; i < extra; i++ {
			time.Sleep(hold / time.Duration(extra+1))
			select {
			case h.signals <- syscall.SIGINT:
			default:
			}
		}
	}()
	return h.run(t)
}

// --- deadlines and stdout --------------------------------------------------

func TestStartupTimeoutFailsTheRun(t *testing.T) {
	h := newHarness(t)
	h.startup = 20 * time.Millisecond
	h.backend.waitErrs = map[string]error{}
	h.backend.stateFn = func(string, int) (ContainerState, error) {
		time.Sleep(200 * time.Millisecond)
		return healthyOn("net", "172.28.0.4"), nil
	}
	got := h.run(t)
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeStartupTimeout) {
		t.Fatalf("code = %v, want startup_timeout", terminal["code"])
	}
	if got.exit != 1 {
		t.Fatalf("exit = %d, want 1", got.exit)
	}
}

// Broken stdout cannot carry a terminal event, so it is diagnosed on stderr
// and exits 1 -- and nothing is created.
func TestBrokenStdoutAtStartedCreatesNothingAndExitsOne(t *testing.T) {
	h := newHarness(t)
	h.stdout = &brokenWriter{}
	got := h.run(t)
	if got.exit != 1 {
		t.Fatalf("exit = %d, want 1", got.exit)
	}
	if h.backend.launchCount() != 0 {
		t.Fatal("a broken control stream still created containers")
	}
	if !strings.Contains(got.stderr, "stdout") {
		t.Fatalf("stderr = %q, want a diagnosis naming stdout", got.stderr)
	}
}

func TestBrokenStdoutAfterStartCleansUpAndExitsOne(t *testing.T) {
	h := newHarness(t)
	h.stdout = &writeThenBreak{limit: 1}
	got := h.run(t)
	if got.exit != 1 {
		t.Fatalf("exit = %d, want 1", got.exit)
	}
	if h.backend.launchCount() == 0 {
		t.Fatal("the run never started a container, so the test proves nothing")
	}
	for _, inst := range h.backend.instances {
		if !inst.wasTerminated() {
			t.Fatalf("container %s survived a broken control stream", inst.id)
		}
	}
}

// writeThenBreak accepts limit writes and then fails, standing in for a
// reader that goes away mid-run.
type writeThenBreak struct {
	limit int
	seen  int
	buf   bytes.Buffer
}

func (w *writeThenBreak) String() string { return w.buf.String() }

func (w *writeThenBreak) Write(p []byte) (int, error) {
	w.seen++
	if w.seen > w.limit {
		return 0, errors.New("broken pipe")
	}
	return w.buf.Write(p)
}

func TestContextCancellationStopsTheFleet(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.backend.stateFn = func(_ string, poll int) (ContainerState, error) {
		if poll >= 1 {
			cancel()
		}
		return healthyOn("net", "172.28.0.4"), nil
	}
	got := h.runCtx(t, ctx)
	for _, inst := range h.backend.instances {
		if !inst.wasTerminated() {
			t.Fatalf("container %s survived cancellation", inst.id)
		}
	}
	if got.terminal(t)["event"] == "" {
		t.Fatalf("no terminal event: %#v", got.events)
	}
}

func TestExactlyOneTerminalEventPerRun(t *testing.T) {
	h := newHarness(t)
	h.signalAfterReady(syscall.SIGTERM)
	got := h.run(t)
	terminals := 0
	for _, ev := range got.events {
		if ev["event"] == "failed" || ev["event"] == "stopped" {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want exactly 1 (%#v)", terminals, got.events)
	}
}

func TestEveryEventCarriesTheProtocolVersionAndRunID(t *testing.T) {
	h := newHarness(t)
	h.signalAfterReady(syscall.SIGTERM)
	got := h.run(t)
	for _, ev := range got.events {
		if int(ev["protocol"].(float64)) != ProtocolVersion {
			t.Fatalf("event %v has protocol %v", ev["event"], ev["protocol"])
		}
		if ev["run_id"] != "run1" {
			t.Fatalf("event %v has run_id %v", ev["event"], ev["run_id"])
		}
	}
}

func TestRunIDIsRandomAndShort(t *testing.T) {
	first, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	second, err := NewRunID()
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	if first == second {
		t.Fatalf("two run IDs agreed: %s", first)
	}
	if len(first) != 8 {
		t.Fatalf("run ID %q is %d characters, want 8", first, len(first))
	}
}

// A Docker daemon that cannot be reached at all is the one failure the
// protocol most needs to report cleanly, and it was the one that crashed:
// validate returned before assigning the backend, and cleanup then called a
// method on the nil interface. The run must report docker_unavailable and
// exit, not panic.
func TestUnreachableDockerBackendReportsInsteadOfPanicking(t *testing.T) {
	h := newHarness(t)
	h.backend = nil
	got := h.run(t)
	if got.exit != 1 {
		t.Fatalf("exit = %d, want 1", got.exit)
	}
	terminal := got.terminal(t)
	if terminal["code"] != string(CodeDockerUnavailable) {
		t.Fatalf("code = %v, want docker_unavailable", terminal["code"])
	}
	if terminal["phase"] != string(PhaseValidate) {
		t.Fatalf("phase = %v, want validate", terminal["phase"])
	}
	if terminal["cleanup_complete"] != true {
		t.Fatalf("cleanup_complete = %v, want true: nothing was ever created", terminal["cleanup_complete"])
	}
}
