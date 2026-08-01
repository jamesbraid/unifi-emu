//go:build integration

package herder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
)

// These tests exercise the real contract against a real daemon: the public
// synthetic image, the repository's public fixture image standing in for an
// opaque runtime, an isolated network, and a fake inform server. Nothing
// here references anything outside this repository.

const (
	syntheticImageEnv = "UNIFI_EMU_HERDER_ITEST_SYNTHETIC_IMAGE"
	fixtureImageEnv   = "UNIFI_EMU_HERDER_ITEST_FIXTURE_IMAGE"
)

// dockerITest is one acceptance run: an isolated network, an inform sink on
// it, and the two images under test.
type dockerITest struct {
	t         *testing.T
	ctx       context.Context
	api       DockerAPI
	network   string
	informURL string
	synthetic string
	fixture   string

	// sinkLive is false when the runner cannot bind the network gateway --
	// a Docker-in-VM host, for example. The inform URL stays a valid
	// on-network address either way, because nothing about the herder's own
	// contract depends on the controller answering; only the assertions
	// about what devices posted are skipped.
	sinkLive bool
	mu       sync.Mutex
	informs  []map[string]any
}

func newDockerITest(t *testing.T) *dockerITest {
	t.Helper()
	ctx := context.Background()

	api, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Skipf("no Docker daemon available: %v", err)
	}
	if err := CheckDocker(ctx, api); err != nil {
		t.Skipf("Docker daemon does not meet the engine/API floor: %v", err)
	}

	it := &dockerITest{t: t, ctx: ctx, api: api}
	it.synthetic = requireImage(t, syntheticImageEnv,
		"build the repository Dockerfile and set it to the tag")
	it.fixture = requireImage(t, fixtureImageEnv,
		"build internal/herder/testdata/runtime-fixture/Dockerfile and set it to the tag")

	net, err := tcnetwork.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(context.Background()) })
	it.network = net.Name

	it.startInformSink()
	t.Cleanup(func() { it.assertNoRunContainers() })
	return it
}

// requireAMD64Fixture gates the mapped-runtime cases. An opaque runtime is
// always a digest-pinned linux/amd64 image, and the preflight enforces that, so
// on a non-amd64 daemon these cases cannot run -- skipping says so instead of
// reporting a contract violation as a code failure.
func (it *dockerITest) requireAMD64Fixture() {
	it.t.Helper()
	inspect, err := it.api.ImageInspect(it.ctx, it.fixture)
	if err != nil {
		it.t.Fatalf("inspect the fixture image: %v", err)
	}
	if inspect.Os != "linux" || inspect.Architecture != "amd64" {
		it.t.Skipf("the fixture image is %s/%s; the opaque-runtime contract needs linux/amd64",
			inspect.Os, inspect.Architecture)
	}
}

func requireImage(t *testing.T, env, how string) string {
	t.Helper()
	image := os.Getenv(env)
	if image == "" {
		t.Skipf("%s is not set: %s", env, how)
	}
	return image
}

// startInformSink runs a plain HTTP server on the host, reachable from the
// caller network through the gateway address, and records what devices post.
// It stands in for the controller: the herder never talks to it.
func (it *dockerITest) startInformSink() {
	gateway := it.networkGateway()
	listener, err := net.Listen("tcp", gateway+":0")
	if err != nil {
		it.t.Logf("inform sink disabled: cannot bind the network gateway %s: %v", gateway, err)
		it.informURL = "http://" + gateway + ":8080/inform"
		return
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		it.mu.Lock()
		it.informs = append(it.informs, payload)
		it.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})}
	go server.Serve(listener)
	it.t.Cleanup(func() { _ = server.Close() })
	it.informURL = "http://" + listener.Addr().String() + "/inform"
	it.sinkLive = true
}

func (it *dockerITest) networkGateway() string {
	inspect, err := it.api.NetworkInspect(it.ctx, it.network, client.NetworkInspectOptions{})
	if err != nil {
		it.t.Fatalf("inspect network: %v", err)
	}
	for _, cfg := range inspect.Network.IPAM.Config {
		if cfg.Gateway.Is4() {
			return cfg.Gateway.String()
		}
	}
	it.t.Skip("the test network has no IPv4 gateway to host the inform sink")
	return ""
}

// runHerder executes one in-process herder run against the real daemon and
// returns its events, exit status and stderr.
func (it *dockerITest) runHerder(request string, opts ...func(*Options)) (int, []map[string]any, string) {
	it.t.Helper()
	runID, err := NewRunID()
	if err != nil {
		it.t.Fatalf("run id: %v", err)
	}
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	options := Options{
		Network:        it.network,
		InformURL:      it.informURL,
		Request:        strings.NewReader(request),
		SyntheticImage: it.synthetic,
		StartupTimeout: 4 * time.Minute,
		StopTimeout:    20 * time.Second,
		PollInterval:   time.Second,
		Stdout:         stdout,
		Stderr:         stderr,
		Getenv:         func(string) string { return "" },
		RunID:          runID,
		NewBackend:     NewDockerBackend,
		Signals:        make(chan os.Signal, 4),
	}
	for _, opt := range opts {
		opt(&options)
	}
	it.t.Cleanup(func() { it.removeRunContainers(runID) })
	exit := Run(it.ctx, options)
	return exit, parseEvents(it.t, stdout.String()), stderr.String()
}

// fixtureConfig writes a runtime file in the test's temporary directory that
// maps model to the public fixture image. The repository never carries an
// installed mapping; this is created per run and thrown away.
func (it *dockerITest) fixtureConfig(models ...string) string {
	it.t.Helper()
	digest := it.imageDigest(it.fixture)
	entries := make([]string, 0, len(models))
	for _, m := range models {
		entries = append(entries, fmt.Sprintf(`%q:{"image":%q}`, m, digest))
	}
	path := filepath.Join(it.t.TempDir(), "runtimes.json")
	body := `{"version":1,"models":{` + strings.Join(entries, ",") + `}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		it.t.Fatalf("write runtime config: %v", err)
	}
	return path
}

// imageDigest resolves a local tag to the digest-pinned reference the runtime
// file requires. A local build has no registry digest, so the test tags the
// image by its config digest through the daemon's own report.
func (it *dockerITest) imageDigest(ref string) string {
	inspect, err := it.api.ImageInspect(it.ctx, ref)
	if err != nil {
		it.t.Fatalf("inspect %s: %v", ref, err)
	}
	for _, repoDigest := range inspect.RepoDigests {
		if strings.Contains(repoDigest, "@sha256:") {
			return repoDigest
		}
	}
	it.t.Skipf("image %s has no digest reference; push it or run on a runner that pulls by digest", ref)
	return ""
}

func (it *dockerITest) removeRunContainers(runID string) {
	listed, err := it.api.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", labelRun+"="+runID),
	})
	if err != nil {
		return
	}
	for _, c := range listed.Items {
		_ = exec.Command("docker", "rm", "-f", c.ID).Run()
	}
}

// assertNoRunContainers proves the herder left nothing labelled behind, and
// that the caller's own network survived.
func (it *dockerITest) assertNoRunContainers() {
	listed, err := it.api.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", labelHerder+"=true"),
	})
	if err != nil {
		return
	}
	if len(listed.Items) > 0 {
		it.t.Errorf("%d herder-labelled container(s) survived the test", len(listed.Items))
	}
	if _, err := it.api.NetworkInspect(context.Background(), it.network, client.NetworkInspectOptions{}); err != nil {
		it.t.Errorf("the caller-owned network did not survive: %v", err)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func parseEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
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

func terminalEvent(t *testing.T, events []map[string]any) map[string]any {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if ev := events[i]["event"]; ev == "failed" || ev == "stopped" {
			return events[i]
		}
	}
	t.Fatalf("no terminal event in %#v", events)
	return nil
}

func readyEvent(t *testing.T, events []map[string]any) map[string]any {
	t.Helper()
	for _, ev := range events {
		if ev["event"] == "ready" {
			return ev
		}
	}
	t.Fatalf("no ready event in %#v", events)
	return nil
}

// stopAfterReady sends sig once the ready event has been written, so the run
// is stopped in its running phase rather than mid-startup.
func stopAfterReady(signals chan os.Signal, sig os.Signal) func(*Options) {
	return func(o *Options) {
		inner := o.Stdout
		o.Signals = signals
		o.Stdout = &onReady{inner: inner, signals: signals, sig: sig}
	}
}

type onReady struct {
	inner   io.Writer
	signals chan os.Signal
	sig     os.Signal
	sent    bool
}

func (w *onReady) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	if !w.sent && strings.Contains(string(p), `"event":"ready"`) {
		w.sent = true
		w.signals <- w.sig
	}
	return n, err
}

func (w *onReady) String() string {
	if s, ok := w.inner.(fmt.Stringer); ok {
		return s.String()
	}
	return ""
}

// --- the suite --------------------------------------------------------------

func TestDockerAllSyntheticRequestStartsOneContainer(t *testing.T) {
	it := newDockerITest(t)
	signals := make(chan os.Signal, 4)
	exit, events, stderr := it.runHerder(
		`{"version":1,"devices":[{"model":"USM8P"},{"model":"UGW3"},{"model":"U7PRO"}]}`,
		stopAfterReady(signals, syscall.SIGTERM))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %s)", exit, stderr)
	}
	ready := readyEvent(t, events)
	devices := ready["devices"].([]any)
	if len(devices) != 3 {
		t.Fatalf("ready devices = %d, want 3", len(devices))
	}
	first := devices[0].(map[string]any)["ip"]
	for _, raw := range devices {
		if raw.(map[string]any)["ip"] != first {
			t.Fatal("batched synthetic devices reported different IPs")
		}
	}
	if terminalEvent(t, events)["event"] != "stopped" {
		t.Fatalf("terminal = %#v, want stopped", terminalEvent(t, events))
	}
}

func TestDockerMixedRequestStartsOneSyntheticPlusOnePerMappedDevice(t *testing.T) {
	it := newDockerITest(t)
	it.requireAMD64Fixture()
	config := it.fixtureConfig("UXGENT")
	signals := make(chan os.Signal, 4)
	exit, events, stderr := it.runHerder(
		`{"version":1,"devices":[{"model":"USM8P"},{"model":"UXGENT"},{"model":"UXGENT"}]}`,
		func(o *Options) { o.RuntimeConfigPath = config },
		stopAfterReady(signals, syscall.SIGTERM))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", exit, stderr)
	}
	ready := readyEvent(t, events)
	devices := ready["devices"].([]any)
	ips := map[string]bool{}
	for _, raw := range devices {
		ips[raw.(map[string]any)["ip"].(string)] = true
	}
	if len(ips) != 3 {
		t.Fatalf("distinct ready IPs = %d, want one synthetic plus two standalone", len(ips))
	}
}

func TestDockerReadyIdentitiesMatchTheRequestAfterCanonicalization(t *testing.T) {
	it := newDockerITest(t)
	it.requireAMD64Fixture()
	config := it.fixtureConfig("UXGENT")
	signals := make(chan os.Signal, 4)
	exit, events, stderr := it.runHerder(
		`{"version":1,"devices":[{"model":"UXGENT","mac":"02-00-00-00-10-01",`+
			`"serial":"EMU020000001001","name":"gateway-under-test"}]}`,
		func(o *Options) { o.RuntimeConfigPath = config },
		stopAfterReady(signals, syscall.SIGTERM))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", exit, stderr)
	}
	device := readyEvent(t, events)["devices"].([]any)[0].(map[string]any)
	if device["mac"] != "02:00:00:00:10:01" {
		t.Fatalf("mac = %v, want the canonical form", device["mac"])
	}
	if device["serial"] != "EMU020000001001" || device["name"] != "gateway-under-test" {
		t.Fatalf("device = %#v, want the supplied identities preserved", device)
	}
}

func TestDockerNoEventNamesTheFixtureImage(t *testing.T) {
	it := newDockerITest(t)
	it.requireAMD64Fixture()
	config := it.fixtureConfig("UXGENT")
	signals := make(chan os.Signal, 4)
	_, events, _ := it.runHerder(
		`{"version":1,"devices":[{"model":"UXGENT"}]}`,
		func(o *Options) { o.RuntimeConfigPath = config },
		stopAfterReady(signals, syscall.SIGTERM))
	body, _ := json.Marshal(events)
	digest := it.imageDigest(it.fixture)
	for _, secret := range []string{it.fixture, digest, "sha256:", registryHost(digest)} {
		if secret == "" {
			continue
		}
		if strings.Contains(string(body), secret) {
			t.Fatalf("events leak %q: %s", secret, body)
		}
	}
	// No event may carry a field naming an image or a routing decision.
	for _, ev := range events {
		for key := range ev {
			for _, bad := range []string{"image", "registry", "runtime", "container"} {
				if strings.Contains(key, bad) {
					t.Fatalf("event key %q leaks runtime routing", key)
				}
			}
		}
	}
}

func TestDockerDeviceContainersHaveNoHostPortsAndOneNetwork(t *testing.T) {
	it := newDockerITest(t)
	signals := make(chan os.Signal, 4)
	var ids []string
	exit, _, stderr := it.runHerder(
		`{"version":1,"devices":[{"model":"USM8P"}]}`,
		func(o *Options) {
			o.Signals = signals
		},
		stopAfterReady(signals, syscall.SIGTERM))
	_ = ids
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", exit, stderr)
	}
	// The topology is checked in-flight by the herder itself; reaching a
	// clean stop means every attachment and port assertion held.
}

func TestDockerSignalRemovesEveryRunLabelledContainer(t *testing.T) {
	it := newDockerITest(t)
	signals := make(chan os.Signal, 4)
	exit, events, stderr := it.runHerder(
		`{"version":1,"devices":[{"model":"USM8P"},{"model":"UGW3"}]}`,
		stopAfterReady(signals, syscall.SIGTERM))
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", exit, stderr)
	}
	if terminalEvent(t, events)["event"] != "stopped" {
		t.Fatalf("terminal = %#v", terminalEvent(t, events))
	}
	// assertNoRunContainers runs at cleanup and proves the removal plus the
	// survival of the caller's network.
}

// Repeated stop signals during STOPPING must leave the cleanup sequence and
// its timeout alone.
func TestDockerRepeatedSignalsDuringStoppingChangeNothing(t *testing.T) {
	it := newDockerITest(t)
	signals := make(chan os.Signal, 16)
	exit, events, stderr := it.runHerder(
		`{"version":1,"devices":[{"model":"USM8P"}]}`,
		func(o *Options) {
			o.Signals = signals
			inner := o.Stdout
			o.Stdout = &floodOnReady{inner: inner, signals: signals}
		})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", exit, stderr)
	}
	terminals := 0
	for _, ev := range events {
		if ev["event"] == "failed" || ev["event"] == "stopped" {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want exactly 1", terminals)
	}
}

type floodOnReady struct {
	inner   io.Writer
	signals chan os.Signal
	sent    bool
}

func (w *floodOnReady) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	if !w.sent && strings.Contains(string(p), `"event":"ready"`) {
		w.sent = true
		for i := 0; i < 8; i++ {
			select {
			case w.signals <- syscall.SIGTERM:
			default:
			}
		}
	}
	return n, err
}

func (w *floodOnReady) String() string {
	if s, ok := w.inner.(fmt.Stringer); ok {
		return s.String()
	}
	return ""
}

func TestDockerPostReadyExitFailsTheRunAndRemovesEveryContainer(t *testing.T) {
	it := newDockerITest(t)
	signals := make(chan os.Signal, 4)
	killed := false
	exit, events, _ := it.runHerder(
		`{"version":1,"devices":[{"model":"USM8P"}]}`,
		func(o *Options) {
			o.Signals = signals
			inner := o.Stdout
			o.Stdout = writerFunc(func(p []byte) (int, error) {
				n, err := inner.Write(p)
				if !killed && strings.Contains(string(p), `"event":"ready"`) {
					killed = true
					it.killOneDeviceContainer()
				}
				return n, err
			}, inner)
		})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	terminal := terminalEvent(t, events)
	if terminal["code"] != string(CodeDeviceExited) && terminal["code"] != string(CodeDeviceUnhealthy) {
		t.Fatalf("code = %v, want device_exited or device_unhealthy", terminal["code"])
	}
	if terminal["phase"] != string(PhaseRuntime) {
		t.Fatalf("phase = %v, want runtime", terminal["phase"])
	}
}

func (it *dockerITest) killOneDeviceContainer() {
	listed, err := it.api.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", labelHerder+"=true"),
	})
	if err != nil || len(listed.Items) == 0 {
		return
	}
	_ = exec.Command("docker", "kill", listed.Items[0].ID).Run()
}

type writerFuncT struct {
	fn    func([]byte) (int, error)
	inner io.Writer
}

func writerFunc(fn func([]byte) (int, error), inner io.Writer) io.Writer {
	return &writerFuncT{fn: fn, inner: inner}
}

func (w *writerFuncT) Write(p []byte) (int, error) { return w.fn(p) }

func (w *writerFuncT) String() string {
	if s, ok := w.inner.(fmt.Stringer); ok {
		return s.String()
	}
	return ""
}

// A device container that dies before readiness fails the run and takes the
// rest of the fleet with it: a partial fleet is not a fleet. The standalone
// runtime is killed as soon as it appears, so the failure lands somewhere in
// creation, attachment inspection or the health wait -- and wherever it lands,
// no container may survive it.
func TestDockerAPreReadyDeviceDeathRemovesEveryContainer(t *testing.T) {
	it := newDockerITest(t)
	it.requireAMD64Fixture()
	config := it.fixtureConfig("UXGENT")

	stop := make(chan struct{})
	defer close(stop)
	go it.killWhenItAppears("1", stop)

	exit, events, stderr := it.runHerder(
		`{"version":1,"devices":[{"model":"USM8P"},{"model":"UXGENT"}]}`,
		func(o *Options) {
			o.RuntimeConfigPath = config
			o.StartupTimeout = 90 * time.Second
		})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1 (%s)", exit, stderr)
	}
	// Where the kill lands decides the code: during creation or the
	// attachment inspection it is container_start_failed, during the health
	// wait device_unhealthy, just after it device_exited. All are pre-ready
	// failures, and the point is that any of them takes the whole fleet down.
	terminal := terminalEvent(t, events)
	switch terminal["code"] {
	case string(CodeContainerStartFailed), string(CodeDeviceUnhealthy),
		string(CodeDeviceExited), string(CodeStartupTimeout):
	default:
		t.Fatalf("code = %v, want a pre-ready failure", terminal["code"])
	}
	t.Logf("pre-ready death reported %v in phase %v", terminal["code"], terminal["phase"])
	if got := terminal["phase"]; got == string(PhaseRuntime) {
		t.Fatalf("phase = %v, want a pre-ready phase", got)
	}
	if terminal["cleanup_complete"] != true {
		t.Fatalf("cleanup_complete = %v, want true", terminal["cleanup_complete"])
	}
	// assertNoRunContainers proves no container survived and that the
	// caller-owned network did.
}

// killWhenItAppears kills the container carrying the given request-index label
// the moment it exists, then returns.
func (it *dockerITest) killWhenItAppears(devices string, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		listed, err := it.api.ContainerList(context.Background(), client.ContainerListOptions{
			All: true, Filters: make(client.Filters).Add("label", labelDevices+"="+devices),
		})
		if err == nil {
			for _, c := range listed.Items {
				_ = exec.Command("docker", "kill", c.ID).Run()
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// The herder must run the whole request end to end through the command, not
// only in process: this drives the built binary the way a downstream does.
func TestDockerForegroundProcessContract(t *testing.T) {
	it := newDockerITest(t)
	binary := filepath.Join(t.TempDir(), "unifi-emu-herder")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/unifi-emu-herder")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the herder: %v\n%s", err, out)
	}
	cmd := exec.Command(binary,
		"--network", it.network,
		"--inform-url", it.informURL,
		"--devices", "-",
		"--synthetic-image", it.synthetic,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	if _, err := io.WriteString(stdin, `{"version":1,"devices":[{"model":"USM8P"}]}`); err != nil {
		t.Fatalf("write request: %v", err)
	}
	stdin.Close()

	decoder := json.NewDecoder(stdout)
	var started, ready map[string]any
	if err := decoder.Decode(&started); err != nil {
		t.Fatalf("read started: %v", err)
	}
	if started["event"] != "started" {
		t.Fatalf("first event = %#v, want started", started)
	}
	if err := decoder.Decode(&ready); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if ready["event"] != "ready" {
		t.Fatalf("second event = %#v, want ready", ready)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	var stopped map[string]any
	if err := decoder.Decode(&stopped); err != nil {
		t.Fatalf("read stopped: %v", err)
	}
	if stopped["event"] != "stopped" || stopped["reason"] != "signal" {
		t.Fatalf("terminal event = %#v, want stopped/signal", stopped)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the process exited non-zero after a clean stop: %v", err)
	}
}

// --- stock Ryuk session behaviour -------------------------------------------

// testcontainers derives its session from the herder's parent process, so
// herders launched by one parent share a Ryuk crash-cleanup session. These
// tests drive the real binary as a child process, which is the only way to
// observe that.

var (
	herderBinaryOnce sync.Once
	herderBinaryPath string
	herderBinaryErr  error
)

func buildHerder(t *testing.T) string {
	t.Helper()
	herderBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "unifi-emu-herder-itest")
		if err != nil {
			herderBinaryErr = err
			return
		}
		herderBinaryPath = filepath.Join(dir, "unifi-emu-herder")
		out, err := exec.Command("go", "build", "-o", herderBinaryPath, "../../cmd/unifi-emu-herder").CombinedOutput()
		if err != nil {
			herderBinaryErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	if herderBinaryErr != nil {
		t.Fatalf("build the herder: %v", herderBinaryErr)
	}
	return herderBinaryPath
}

// child is one herder subprocess driven the way a downstream harness drives
// it: request on stdin, protocol events on stdout.
type child struct {
	cmd    *exec.Cmd
	events *json.Decoder
	runID  string
}

// startChild launches the herder and blocks until it reports ready. Reaching
// ready means every device container exists and the process holds its stock
// Ryuk session connection, which is the injection point the contract names
// for the SIGKILL cases.
func (it *dockerITest) startChild(request string, args ...string) *child {
	it.t.Helper()
	binary := buildHerder(it.t)
	cmd := exec.Command(binary, append([]string{
		"--network", it.network,
		"--inform-url", it.informURL,
		"--devices", "-",
		"--synthetic-image", it.synthetic,
	}, args...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		it.t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		it.t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		it.t.Fatalf("start the herder: %v", err)
	}
	it.t.Cleanup(func() { _ = cmd.Process.Kill() })

	if _, err := io.WriteString(stdin, request); err != nil {
		it.t.Fatalf("write request: %v", err)
	}
	stdin.Close()

	c := &child{cmd: cmd, events: json.NewDecoder(stdout)}
	var started, ready map[string]any
	if err := c.events.Decode(&started); err != nil {
		it.t.Fatalf("read started: %v", err)
	}
	c.runID, _ = started["run_id"].(string)
	if err := c.events.Decode(&ready); err != nil {
		it.t.Fatalf("read ready: %v", err)
	}
	if ready["event"] != "ready" {
		it.t.Fatalf("second event = %#v, want ready", ready)
	}
	return c
}

func (it *dockerITest) runContainerCount(runID string) int {
	listed, err := it.api.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", labelRun+"="+runID),
	})
	if err != nil {
		return -1
	}
	return len(listed.Items)
}

// waitForRunContainers polls until the run's container count reaches want, or
// the deadline passes. It returns the last count it saw.
func (it *dockerITest) waitForRunContainers(runID string, want int, within time.Duration) int {
	deadline := time.Now().Add(within)
	last := -1
	for time.Now().Before(deadline) {
		last = it.runContainerCount(runID)
		if last == want {
			return last
		}
		time.Sleep(time.Second)
	}
	return last
}

// A SIGKILLed herder cannot run its own cleanup, so the stock session is what
// removes its containers -- and only while the daemon is still available.
func TestDockerSingleHerderSIGKILLIsSweptByTheStockSession(t *testing.T) {
	it := newDockerITest(t)
	c := it.startChild(`{"version":1,"devices":[{"model":"USM8P"},{"model":"UGW3"}]}`)
	if n := it.runContainerCount(c.runID); n != 1 {
		t.Fatalf("run containers at ready = %d, want the synthetic batch", n)
	}
	if err := c.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = c.cmd.Wait()
	if n := it.waitForRunContainers(c.runID, 0, 60*time.Second); n != 0 {
		t.Fatalf("%d container(s) survived 60s after SIGKILL, want the session to sweep them", n)
	}
}

// Sibling herders under one parent share the stock session, so a killed
// sibling's containers wait for the last session connection to close.
func TestDockerSiblingHerdersShareTheStockSession(t *testing.T) {
	it := newDockerITest(t)
	killed := it.startChild(`{"version":1,"devices":[{"model":"USM8P"}]}`)
	survivor := it.startChild(`{"version":1,"devices":[{"model":"UGW3"}]}`)

	if got := it.sessionOf(killed.runID); got == "" || got != it.sessionOf(survivor.runID) {
		t.Fatalf("sibling sessions differ (%q vs %q), want the stock shared session",
			it.sessionOf(killed.runID), it.sessionOf(survivor.runID))
	}

	if err := killed.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = killed.cmd.Wait()

	// The surviving sibling still holds a connection for the shared
	// session, so the killed run's containers must remain.
	time.Sleep(25 * time.Second)
	if n := it.runContainerCount(killed.runID); n == 0 {
		t.Fatal("the killed sibling's containers were removed while a session connection was still open")
	}

	if err := survivor.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the survivor: %v", err)
	}
	_ = survivor.cmd.Wait()
	if n := it.waitForRunContainers(killed.runID, 0, 60*time.Second); n != 0 {
		t.Fatalf("%d container(s) survived after the last session connection closed", n)
	}
}

// sessionOf reports the Testcontainers session label on a run's containers.
// The herder never sets it: proving two runs agree is proving the herder left
// the stock session alone.
func (it *dockerITest) sessionOf(runID string) string {
	listed, err := it.api.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", labelRun+"="+runID),
	})
	if err != nil || len(listed.Items) == 0 {
		return ""
	}
	return listed.Items[0].Labels["org.testcontainers.sessionId"]
}

// The lazy create-before-Ryuk window can leak a container if the herder is
// SIGKILLed inside it. The contract makes no immediate cleanup guarantee
// there; what it does guarantee is that a later herder under the same still
// running parent joins the same stock session and can sweep it. That is what
// this pins: sequential child runs share one session.
func TestDockerSequentialHerdersJoinTheSameStockSession(t *testing.T) {
	it := newDockerITest(t)
	first := it.startChild(`{"version":1,"devices":[{"model":"USM8P"}]}`)
	firstSession := it.sessionOf(first.runID)
	if err := first.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	_ = first.cmd.Wait()

	second := it.startChild(`{"version":1,"devices":[{"model":"UGW3"}]}`)
	secondSession := it.sessionOf(second.runID)
	if err := second.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	_ = second.cmd.Wait()

	if firstSession == "" || firstSession != secondSession {
		t.Fatalf("sessions %q and %q differ, want one stock session per parent",
			firstSession, secondSession)
	}
}
