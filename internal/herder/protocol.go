// Package herder plans and runs fake UniFi devices in Docker containers on
// behalf of a downstream test harness. Its supported integration surface is
// the foreground NDJSON protocol on stdout, not a Go API: the package stays
// internal so downstream repositories consume the process, not the types.
package herder

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersion is the stdout control-protocol version every event carries.
// Fields may be added within a version; removing one or changing its meaning
// requires a new version.
const ProtocolVersion = 1

// Phase names the stage a failure happened in.
type Phase string

const (
	PhaseValidate Phase = "validate"
	PhasePull     Phase = "pull"
	PhaseStart    Phase = "start"
	PhaseHealth   Phase = "health"
	PhaseRuntime  Phase = "runtime"
	PhaseCleanup  Phase = "cleanup"
)

// Code is a stable public failure code. Downstream code matches on these
// rather than on message text, so a code's meaning never changes.
type Code string

const (
	CodeInvalidRequest           Code = "invalid_request"
	CodeInvalidRuntimeConfig     Code = "invalid_runtime_config"
	CodeNetworkNotFound          Code = "network_not_found"
	CodeInformURLInvalid         Code = "inform_url_invalid"
	CodeDockerVersionUnsupported Code = "docker_version_unsupported"
	CodeReaperDisabled           Code = "reaper_disabled"
	CodeSyntheticModelUnknown    Code = "synthetic_model_unknown"
	CodeSyntheticImageRequired   Code = "synthetic_image_required"
	CodeImagePullFailed          Code = "image_pull_failed"
	CodeStartupTimeout           Code = "startup_timeout"
	CodeImageInvalid             Code = "image_invalid"
	CodeCapabilityUnsupported    Code = "capability_unsupported"
	CodeContainerStartFailed     Code = "container_start_failed"
	CodeEndpointMACMismatch      Code = "endpoint_mac_mismatch"
	CodeDeviceUnhealthy          Code = "device_unhealthy"
	CodeDeviceExited             Code = "device_exited"
	CodeDeviceRestarted          Code = "device_restarted"
	CodeNetworkDetached          Code = "network_detached"
	CodeDockerUnavailable        Code = "docker_unavailable"
	CodeCleanupFailed            Code = "cleanup_failed"
)

// codeSpec is one row of the protocol-1 code/phase/exit matrix: where a code
// may legally be reported and the stable public summary that goes with it.
// Raw Docker errors, registry hosts and image references never reach stdout,
// so the summary is the only downstream-facing text.
type codeSpec struct {
	phases  []Phase
	exit    int
	message string
}

var codeMatrix = map[Code]codeSpec{
	CodeInvalidRequest: {[]Phase{PhaseValidate}, 1,
		"the device request is not valid"},
	CodeInvalidRuntimeConfig: {[]Phase{PhaseValidate}, 1,
		"the runtime configuration is not valid"},
	CodeNetworkNotFound: {[]Phase{PhaseValidate}, 1,
		"the requested Docker network does not exist"},
	CodeInformURLInvalid: {[]Phase{PhaseValidate}, 1,
		"the inform URL is not a canonical IPv4 inform endpoint"},
	CodeDockerVersionUnsupported: {[]Phase{PhaseValidate}, 1,
		"the Docker daemon is older than the supported engine and API versions"},
	CodeReaperDisabled: {[]Phase{PhaseValidate}, 1,
		"the Testcontainers reaper is disabled"},
	CodeSyntheticModelUnknown: {[]Phase{PhaseValidate}, 1,
		"a requested model has no synthetic profile"},
	CodeSyntheticImageRequired: {[]Phase{PhaseValidate}, 1,
		"the plan needs a synthetic image and none was selected"},
	CodeImagePullFailed: {[]Phase{PhasePull}, 1,
		"a required device image could not be pulled"},
	CodeStartupTimeout: {[]Phase{PhaseValidate, PhasePull, PhaseStart, PhaseHealth}, 1,
		"startup did not complete before the startup timeout"},
	CodeImageInvalid: {[]Phase{PhasePull}, 1,
		"a required device image does not satisfy the device-container contract"},
	CodeCapabilityUnsupported: {[]Phase{PhaseValidate}, 1,
		"the Docker daemon cannot apply the configured capability set"},
	CodeContainerStartFailed: {[]Phase{PhaseStart}, 1,
		"a device container could not be created or started as required"},
	CodeEndpointMACMismatch: {[]Phase{PhaseStart}, 1,
		"a device container endpoint MAC does not match the requested MAC"},
	CodeDeviceUnhealthy: {[]Phase{PhaseHealth, PhaseRuntime}, 1,
		"one or more device runtimes did not become healthy"},
	CodeDeviceExited: {[]Phase{PhaseHealth, PhaseRuntime}, 1,
		"one or more device containers exited"},
	CodeDeviceRestarted: {[]Phase{PhaseRuntime}, 1,
		"one or more device containers restarted"},
	CodeNetworkDetached: {[]Phase{PhaseRuntime}, 1,
		"a device container network attachment changed"},
	CodeDockerUnavailable: {[]Phase{PhaseValidate, PhasePull, PhaseStart, PhaseHealth, PhaseRuntime}, 1,
		"the Docker API became unavailable"},
	CodeCleanupFailed: {[]Phase{PhaseCleanup}, 1,
		"one or more device containers remain after cleanup"},
}

// Codes returns every code defined by this protocol version.
func Codes() []Code {
	out := make([]Code, 0, len(codeMatrix))
	for c := range codeMatrix {
		out = append(out, c)
	}
	return out
}

// AllowsPhase reports whether the code may be reported in phase.
func (c Code) AllowsPhase(phase Phase) bool {
	for _, p := range codeMatrix[c].phases {
		if p == phase {
			return true
		}
	}
	return false
}

// ExitStatus is the process exit status a run ending in this code uses.
func (c Code) ExitStatus() int { return codeMatrix[c].exit }

// Message is the stable public summary emitted with this code.
func (c Code) Message() string { return codeMatrix[c].message }

// Identity is one device's public identity, as it appears in every event.
// It never carries an IP: only the ready event reports addresses.
type Identity struct {
	Index  int    `json:"index"`
	Model  string `json:"model"`
	MAC    string `json:"mac"`
	Serial string `json:"serial"`
	Name   string `json:"name"`
}

// ReadyDevice is a ready-event entry: an identity plus the IPv4 address
// Docker assigned on the caller's network, read back by inspection rather
// than trusted from a child process.
type ReadyDevice struct {
	Identity
	IP string `json:"ip"`
}

// FailedEvent is the terminal failure event's payload.
type FailedEvent struct {
	RunID           string
	Phase           Phase
	Code            Code
	Cause           Code // set only for cleanup_failed
	CleanupComplete bool
	Devices         []Identity
}

// wire shapes. Separate structs per event keep the encoded field set exact:
// no event grows a field because another one needed it.
type startedWire struct {
	Protocol int    `json:"protocol"`
	Event    string `json:"event"`
	RunID    string `json:"run_id"`
}

type readyWire struct {
	Protocol int           `json:"protocol"`
	Event    string        `json:"event"`
	RunID    string        `json:"run_id"`
	Devices  []ReadyDevice `json:"devices"`
}

type failedWire struct {
	Protocol        int        `json:"protocol"`
	Event           string     `json:"event"`
	RunID           string     `json:"run_id"`
	Phase           Phase      `json:"phase"`
	Code            Code       `json:"code"`
	Cause           Code       `json:"cause,omitempty"`
	Message         string     `json:"message"`
	CleanupComplete bool       `json:"cleanup_complete"`
	Devices         []Identity `json:"devices"`
}

type stoppedWire struct {
	Protocol int    `json:"protocol"`
	Event    string `json:"event"`
	RunID    string `json:"run_id"`
	Reason   string `json:"reason"`
}

// ErrBrokenStdout reports that the control stream can no longer be written.
// It has no failure code: a broken stdout cannot carry a failed event, so the
// herder diagnoses it on stderr and exits 1.
var ErrBrokenStdout = errors.New("herder: stdout is no longer writable")

// Writer emits the NDJSON control protocol and enforces its ordering rules:
// started exactly once, ready at most once and only before the terminal
// event, and exactly one terminal event. Once a write fails the stream is
// latched broken and nothing further is attempted, so a closed pipe costs one
// failed syscall rather than one per event.
type Writer struct {
	mu       sync.Mutex
	w        io.Writer
	broken   bool
	started  bool
	ready    bool
	terminal bool
}

// NewWriter returns a Writer emitting to w, which is the process stdout in
// production. Each event is written in a single call, so it reaches the
// reader whole before the state machine continues.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Broken reports whether a previous emit failed.
func (w *Writer) Broken() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.broken
}

// Terminal reports whether a terminal event has been emitted.
func (w *Writer) Terminal() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.terminal
}

// Started emits the run's first event. It precedes request decoding so a
// downstream can name the run even when validation fails.
func (w *Writer) Started(runID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return errors.New("herder: started already emitted")
	}
	if err := w.emit(startedWire{ProtocolVersion, "started", runID}); err != nil {
		return err
	}
	w.started = true
	return nil
}

// Ready emits the single ready event, after every planned container is
// healthy and its address has been inspected.
func (w *Writer) Ready(runID string, devices []ReadyDevice) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		return errors.New("herder: ready before started")
	}
	if w.ready {
		return errors.New("herder: ready already emitted")
	}
	if w.terminal {
		return errors.New("herder: ready after the terminal event")
	}
	if devices == nil {
		devices = []ReadyDevice{}
	}
	if err := w.emit(readyWire{ProtocolVersion, "ready", runID, devices}); err != nil {
		return err
	}
	w.ready = true
	return nil
}

// Failed emits the terminal failure event. The code/phase pairing is checked
// against the protocol matrix: an out-of-matrix pairing is a herder bug and
// is refused rather than published.
func (w *Writer) Failed(ev FailedEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.beforeTerminal(); err != nil {
		return err
	}
	if !ev.Code.AllowsPhase(ev.Phase) {
		return fmt.Errorf("herder: code %s is not defined for phase %s", ev.Code, ev.Phase)
	}
	devices := ev.Devices
	if devices == nil {
		devices = []Identity{}
	}
	if err := w.emit(failedWire{
		Protocol: ProtocolVersion, Event: "failed", RunID: ev.RunID,
		Phase: ev.Phase, Code: ev.Code, Cause: ev.Cause,
		Message: ev.Code.Message(), CleanupComplete: ev.CleanupComplete,
		Devices: devices,
	}); err != nil {
		return err
	}
	w.terminal = true
	return nil
}

// Stopped emits the terminal clean-stop event. It means a caller-requested
// signal was handled and cleanup succeeded.
func (w *Writer) Stopped(runID, reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.beforeTerminal(); err != nil {
		return err
	}
	if err := w.emit(stoppedWire{ProtocolVersion, "stopped", runID, reason}); err != nil {
		return err
	}
	w.terminal = true
	return nil
}

func (w *Writer) beforeTerminal() error {
	if !w.started {
		return errors.New("herder: terminal event before started")
	}
	if w.terminal {
		return errors.New("herder: terminal event already emitted")
	}
	return nil
}

func (w *Writer) emit(v any) error {
	if w.broken {
		return ErrBrokenStdout
	}
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.w.Write(append(body, '\n')); err != nil {
		w.broken = true
		return fmt.Errorf("%w: %v", ErrBrokenStdout, err)
	}
	return nil
}

// Failure is an internal error carrying the public code and phase to report
// alongside the private detail. The detail explains the cause on stderr; only
// the code's stable summary reaches stdout.
type Failure struct {
	Code  Code
	Phase Phase
	// Devices are the public identities the failed event names, when the
	// failure belongs to a particular unit rather than to the request or
	// the daemon.
	Devices []Identity
	Detail  string
	Err     error
}

func (f *Failure) Error() string {
	if f.Detail == "" {
		return string(f.Code)
	}
	return string(f.Code) + ": " + f.Detail
}

func (f *Failure) Unwrap() error { return f.Err }

// Message is the stable public summary for this failure.
func (f *Failure) Message() string { return f.Code.Message() }

// failf builds a Failure whose detail is formatted for stderr only.
func failf(code Code, phase Phase, format string, args ...any) *Failure {
	return &Failure{Code: code, Phase: phase, Detail: fmt.Sprintf(format, args...)}
}

// wrapf builds a Failure that keeps err in the chain for stderr diagnosis.
func wrapf(err error, code Code, phase Phase, format string, args ...any) *Failure {
	f := failf(code, phase, format, args...)
	f.Err = err
	return f
}

// asFailure extracts the Failure an error carries, if any.
func asFailure(err error) (*Failure, bool) {
	var f *Failure
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}
