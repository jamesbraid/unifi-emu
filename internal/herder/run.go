package herder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Exit statuses. 2 is reserved for CLI usage errors detected before protocol
// mode begins; everything after `started` is 0 or 1.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// dockerFailureLimit is how many consecutive inspect failures mean the Docker
// API is gone rather than briefly busy. A success resets the count.
const dockerFailureLimit = 3

// cleanupSlack is the headroom cleanup gets on top of --stop-timeout, so the
// force-remove that follows a timed-out stop still has room to run.
const cleanupSlack = 30 * time.Second

// errStopped reports that a signal or caller cancellation ended the run
// cleanly. It is not a *Failure: there is nothing to report but the stop.
var errStopped = errors.New("herder: stopped")

// Instance is one running device container, as the lifecycle layer exposes
// it. Terminate performs the whole documented sequence for one container:
// stop, wait up to the stop timeout, then force-remove.
type Instance interface {
	ID() string
	Terminate(ctx context.Context, stopTimeout time.Duration) error
	Logs(ctx context.Context) (io.ReadCloser, error)
}

// Backend is the Docker-facing half of a run. run.go owns the state machine
// and the protocol; the backend owns the daemon. Splitting them is what lets
// every failure code, phase and cleanup rule be exercised without Docker.
type Backend interface {
	CheckDaemon(ctx context.Context) error
	CheckNetwork(ctx context.Context, name string) error
	CheckCapabilities(ctx context.Context, plan Plan) error
	Prepare(ctx context.Context, plan Plan) error
	Launch(ctx context.Context, plan Plan, unit Unit) (Instance, error)
	WaitHealthy(ctx context.Context, inst Instance) error
	Inspect(ctx context.Context, id string) (ContainerState, error)
	Remaining(ctx context.Context, runID string) (int, error)
}

// Options is one run, already resolved from the command line.
type Options struct {
	Network           string
	InformURL         string
	Request           io.Reader
	RuntimeConfigPath string
	SyntheticImage    string
	StartupTimeout    time.Duration
	StopTimeout       time.Duration
	PollInterval      time.Duration
	Stdout            io.Writer
	Stderr            io.Writer
	Getenv            func(string) string
	Rand              io.Reader
	RunID             string
	Backend           Backend
	// NewBackend builds the Docker backend when Backend is nil. It runs
	// after `started` is flushed so a daemon that is simply not there is
	// reported as a protocol failure rather than as CLI usage.
	NewBackend  func(ctx context.Context) (Backend, error)
	CheckReaper func() error
	Signals     <-chan os.Signal
}

// NewRunID returns the short random identifier every event carries.
func NewRunID() (string, error) {
	var raw [4]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", fmt.Errorf("herder: allocate run id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// Run executes one herder run and returns the process exit status.
func Run(ctx context.Context, opts Options) int {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.CheckReaper == nil {
		opts.CheckReaper = CheckEffectiveReaper
	}
	r := &run{
		opts:   opts,
		out:    NewWriter(opts.Stdout),
		logs:   newLogSink(opts.Stderr, newRedactor(RuntimeConfig{})),
		redact: newRedactor(RuntimeConfig{}),
		phase:  PhaseValidate,
	}
	return r.execute(ctx)
}

// live is one launched unit and the state it was ready in.
type live struct {
	unit     Unit
	inst     Instance
	baseline ContainerState
	ip       string
}

type run struct {
	opts   Options
	out    *Writer
	logs   *logSink
	redact *redactor
	phase  Phase

	// logCtx outlives the startup deadline: device output is evidence for
	// the whole run, not only for its first five minutes.
	logCtx context.Context

	mu sync.Mutex
	// stopped records that a signal or cancellation asked for a clean stop.
	// The first one cancels; every later one is drained and ignored, which is
	// what keeps a repeated SIGINT from restarting cleanup or shortening the
	// stop timeout.
	stopped bool
	units   []*live
}

func (r *run) execute(parent context.Context) int {
	// `started` precedes request decoding so a downstream can name the run
	// even when validation fails. If it cannot be written there is no
	// control stream at all, and nothing has been created yet.
	if err := r.out.Started(r.opts.RunID); err != nil {
		r.diagf("cannot write the control protocol on stdout: %v", err)
		return ExitFailure
	}

	// The startup deadline starts after `started` is flushed and covers
	// validation, pulls, creation, start and readiness.
	signalCtx, stopSignals := context.WithCancel(parent)
	defer stopSignals()
	r.logCtx = signalCtx
	go r.watchSignals(signalCtx, stopSignals)

	startCtx, cancelStart := context.WithTimeout(signalCtx, r.opts.StartupTimeout)
	defer cancelStart()

	plan, err := r.validate(startCtx)
	if err != nil {
		return r.finish(parent, err)
	}
	if err := r.start(startCtx, plan); err != nil {
		return r.finish(parent, err)
	}
	if err := r.out.Ready(r.opts.RunID, r.readyDevices()); err != nil {
		r.diagf("cannot write the ready event: %v", err)
		r.finish(parent, errStopped)
		return ExitFailure
	}
	return r.finish(parent, r.monitor(signalCtx, plan))
}

// watchSignals turns the first stop signal into cancellation. Later signals
// are drained and ignored: once the herder is stopping, another SIGINT must
// not restart cleanup, abort it, or shorten the stop timeout.
func (r *run) watchSignals(ctx context.Context, cancel context.CancelFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case sig, ok := <-r.opts.Signals:
			if !ok {
				return
			}
			r.mu.Lock()
			first := !r.stopped
			r.stopped = true
			r.mu.Unlock()
			if first {
				r.diagf("%v received, stopping", sig)
				cancel()
			}
		}
	}
}

func (r *run) validate(ctx context.Context) (Plan, error) {
	r.phase = PhaseValidate

	if r.opts.Backend == nil {
		backend, err := r.opts.NewBackend(ctx)
		if err != nil {
			return Plan{}, r.classify(err, CodeDockerUnavailable, PhaseValidate)
		}
		r.opts.Backend = backend
	}

	req, err := DecodeRequest(r.opts.Request)
	if err != nil {
		return Plan{}, err
	}
	cfg, err := ResolveRuntimeConfig(r.opts.RuntimeConfigPath, r.opts.Getenv)
	if err != nil {
		return Plan{}, err
	}
	// Every later diagnostic goes through this redactor, so a runtime
	// reference cannot reach stderr from the moment the mapping is known.
	r.redact = newRedactor(cfg)
	r.logs = newLogSink(r.opts.Stderr, r.redact)

	devices, err := Allocate(req.Devices, r.opts.Rand)
	if err != nil {
		return Plan{}, err
	}
	if err := CheckInformURL(r.opts.InformURL); err != nil {
		return Plan{}, err
	}
	// Effective Testcontainers behaviour is read before anything is
	// created: crash cleanup cannot be downgraded by inherited state.
	if err := r.opts.CheckReaper(); err != nil {
		return Plan{}, err
	}
	if err := r.opts.Backend.CheckDaemon(ctx); err != nil {
		return Plan{}, r.classify(err, CodeDockerUnavailable, PhaseValidate)
	}
	if err := r.opts.Backend.CheckNetwork(ctx, r.opts.Network); err != nil {
		return Plan{}, r.classify(err, CodeNetworkNotFound, PhaseValidate)
	}
	plan, err := BuildPlan(PlanInput{
		RunID: r.opts.RunID, Network: r.opts.Network, InformURL: r.opts.InformURL,
		Devices: devices, Runtime: cfg, SyntheticImage: r.opts.SyntheticImage,
	})
	if err != nil {
		return Plan{}, err
	}
	if err := r.opts.Backend.CheckCapabilities(ctx, plan); err != nil {
		return Plan{}, r.classify(err, CodeCapabilityUnsupported, PhaseValidate)
	}

	// All images before any container: a registry failure here leaves no
	// half-started fleet to reconcile.
	r.phase = PhasePull
	if err := r.opts.Backend.Prepare(ctx, plan); err != nil {
		return Plan{}, r.classify(err, CodeImagePullFailed, PhasePull)
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, r.classify(err, CodeStartupTimeout, PhasePull)
	}
	return plan, nil
}

func (r *run) start(ctx context.Context, plan Plan) error {
	r.phase = PhaseStart

	type launched struct {
		unit Unit
		inst Instance
		err  error
	}
	results := make([]launched, len(plan.Units))
	var wg sync.WaitGroup
	for i, unit := range plan.Units {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inst, err := r.opts.Backend.Launch(ctx, plan, unit)
			results[i] = launched{unit: unit, inst: inst, err: err}
		}()
	}
	wg.Wait()

	// Record everything that came up before reporting a failure, so a
	// partial start is rolled back completely rather than half-abandoned.
	var first error
	for _, res := range results {
		if res.inst != nil {
			r.addUnit(&live{unit: res.unit, inst: res.inst})
			go r.logs.follow(r.logCtx, res.unit.DeviceIndices(), res.inst)
		}
		if res.err != nil && first == nil {
			first = r.classifyUnit(res.err, CodeContainerStartFailed, PhaseStart, res.unit)
		}
	}
	if first != nil {
		return first
	}
	if err := ctx.Err(); err != nil {
		return r.classify(err, CodeStartupTimeout, PhaseStart)
	}

	// Health is Docker's verdict, waited on through the stock strategy.
	r.phase = PhaseHealth
	units := r.snapshot()
	waitErrs := make([]error, len(units))
	for i, lu := range units {
		wg.Add(1)
		go func() {
			defer wg.Done()
			waitErrs[i] = r.opts.Backend.WaitHealthy(ctx, lu.inst)
		}()
	}
	wg.Wait()
	for i, err := range waitErrs {
		if err != nil {
			return r.classifyUnit(err, CodeDeviceUnhealthy, PhaseHealth, units[i].unit)
		}
	}
	if err := ctx.Err(); err != nil {
		return r.classify(err, CodeStartupTimeout, PhaseHealth)
	}

	// Only now is the topology inspected: exactly the caller's network, no
	// host port binding, and the requested endpoint MAC where one applies.
	r.phase = PhaseStart
	for _, lu := range units {
		state, err := r.opts.Backend.Inspect(ctx, lu.inst.ID())
		if err != nil {
			return r.classifyUnit(err, CodeDockerUnavailable, PhaseStart, lu.unit)
		}
		ip, err := checkAttachment(plan, lu.unit, state)
		if err != nil {
			return r.withDevices(err, lu.unit)
		}
		lu.baseline = state
		lu.ip = ip
	}
	if err := ctx.Err(); err != nil {
		return r.classify(err, CodeStartupTimeout, PhaseStart)
	}
	return nil
}

func (r *run) monitor(ctx context.Context, plan Plan) error {
	r.phase = PhaseRuntime
	ticker := time.NewTicker(r.opts.PollInterval)
	defer ticker.Stop()

	consecutive := 0
	for {
		if err := ctx.Err(); err != nil {
			return r.classify(err, CodeDockerUnavailable, PhaseRuntime)
		}
		select {
		case <-ctx.Done():
			return r.classify(ctx.Err(), CodeDockerUnavailable, PhaseRuntime)
		case <-ticker.C:
		}
		lost := false
		for _, lu := range r.snapshot() {
			state, err := r.opts.Backend.Inspect(ctx, lu.inst.ID())
			if err != nil {
				consecutive++
				if consecutive >= dockerFailureLimit {
					return r.classifyUnit(err, CodeDockerUnavailable, PhaseRuntime, lu.unit)
				}
				lost = true
				break
			}
			if err := checkRuntimeState(plan, lu.unit, lu.baseline, state); err != nil {
				return r.withDevices(err, lu.unit)
			}
		}
		if !lost {
			consecutive = 0
		}
	}
}

// finish runs the one cleanup path every ending shares and emits the single
// terminal event. Cleanup failure outranks whatever started it: a run that
// leaves a container behind is a worse outcome for the next test than the
// failure that triggered the teardown.
func (r *run) finish(parent context.Context, cause error) int {
	// Cleanup outlives the cancelled run context on purpose: a signal must
	// still remove the containers it interrupted.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), r.opts.StopTimeout+cleanupSlack)
	defer cancel()

	complete, dockerLost := r.cleanup(ctx)
	failure, _ := asFailure(cause)

	switch {
	case !complete:
		code := Code("")
		if failure != nil {
			code = failure.Code
		} else if dockerLost {
			code = CodeDockerUnavailable
		}
		r.emitFailed(FailedEvent{
			RunID: r.opts.RunID, Phase: PhaseCleanup, Code: CodeCleanupFailed,
			Cause: code, CleanupComplete: false, Devices: devicesOf(failure),
		})
		return ExitFailure
	case failure != nil:
		r.emitFailed(FailedEvent{
			RunID: r.opts.RunID, Phase: failure.Phase, Code: failure.Code,
			CleanupComplete: true, Devices: failure.Devices,
		})
		return failure.Code.ExitStatus()
	default:
		if err := r.out.Stopped(r.opts.RunID, r.stopReason()); err != nil {
			r.diagf("cannot write the stopped event: %v", err)
			return ExitFailure
		}
		return ExitOK
	}
}

// cleanup stops and removes every container this run owns, then confirms none
// remains. It never touches the caller's controller or network.
func (r *run) cleanup(ctx context.Context) (complete, dockerLost bool) {
	units := r.snapshot()
	errs := make([]error, len(units))
	var wg sync.WaitGroup
	for i, lu := range units {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = lu.inst.Terminate(ctx, r.opts.StopTimeout)
		}()
	}
	wg.Wait()

	complete = true
	for i, err := range errs {
		if err != nil {
			complete = false
			r.diagf("terminate unit %s: %v", units[i].unit.DeviceIndices(), r.redact.redact(err.Error()))
		}
	}
	remaining, err := r.opts.Backend.Remaining(ctx, r.opts.RunID)
	switch {
	case err != nil:
		r.diagf("confirm cleanup: %v", r.redact.redact(err.Error()))
		return false, true
	case remaining > 0:
		r.diagf("%d container(s) still carry the run label after cleanup", remaining)
		return false, false
	}
	return complete, false
}

func (r *run) emitFailed(ev FailedEvent) {
	if err := r.out.Failed(ev); err != nil {
		r.diagf("cannot write the failed event (%s/%s): %v", ev.Code, ev.Phase, err)
	}
}

// classify turns a backend error into the failure the protocol reports.
// A deadline is the startup timeout wherever it lands; a cancellation is a
// clean stop, because the only thing that cancels this context is a signal or
// the caller going away.
func (r *run) classify(err error, fallback Code, phase Phase) error {
	if err == nil {
		return nil
	}
	if f, ok := asFailure(err); ok {
		return f
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failf(CodeStartupTimeout, phase, "startup deadline expired during %s", phase)
	}
	if errors.Is(err, context.Canceled) {
		return errStopped
	}
	return wrapf(err, fallback, phase, "%v", err)
}

func (r *run) classifyUnit(err error, fallback Code, phase Phase, unit Unit) error {
	return r.withDevices(r.classify(err, fallback, phase), unit)
}

// withDevices attaches the failing unit's public identities to a failure, so
// the failed event names which devices the run lost.
func (r *run) withDevices(err error, unit Unit) error {
	if f, ok := asFailure(err); ok && len(f.Devices) == 0 {
		f.Devices = unit.Devices
	}
	return err
}

func devicesOf(f *Failure) []Identity {
	if f == nil {
		return nil
	}
	return f.Devices
}

func (r *run) readyDevices() []ReadyDevice {
	var out []ReadyDevice
	for _, lu := range r.snapshot() {
		for _, d := range lu.unit.Devices {
			out = append(out, ReadyDevice{Identity: d, IP: lu.ip})
		}
	}
	sortReadyByIndex(out)
	return out
}

func sortReadyByIndex(devices []ReadyDevice) {
	for i := 1; i < len(devices); i++ {
		for j := i; j > 0 && devices[j].Index < devices[j-1].Index; j-- {
			devices[j], devices[j-1] = devices[j-1], devices[j]
		}
	}
}

func (r *run) stopReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return "signal"
	}
	return "canceled"
}

func (r *run) addUnit(lu *live) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.units = append(r.units, lu)
}

func (r *run) snapshot() []*live {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*live(nil), r.units...)
}

// diagf writes one redacted diagnostic to stderr. Stdout carries the protocol
// and nothing else.
func (r *run) diagf(format string, args ...any) {
	if r.opts.Stderr == nil {
		return
	}
	// stderr is best effort: a run whose diagnostics cannot be written is
	// still reported by its terminal event and exit status.
	fmt.Fprintln(r.opts.Stderr, "unifi-emu-herder: "+r.redact.redact(fmt.Sprintf(format, args...))) //nolint:errcheck
}
