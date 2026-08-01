package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/internal/herder"
)

// stubBackend answers every daemon call successfully and never creates
// anything, so the command's own behaviour can be measured without Docker.
type stubBackend struct{ launches int }

func (s *stubBackend) CheckDaemon(context.Context) error                    { return nil }
func (s *stubBackend) CheckNetwork(context.Context, string) error           { return nil }
func (s *stubBackend) CheckCapabilities(context.Context, herder.Plan) error { return nil }
func (s *stubBackend) Prepare(context.Context, herder.Plan) error           { return nil }
func (s *stubBackend) Remaining(context.Context, string) (int, error)       { return 0, nil }
func (s *stubBackend) WaitHealthy(context.Context, herder.Instance) error   { return nil }
func (s *stubBackend) Inspect(context.Context, string) (herder.ContainerState, error) {
	return herder.ContainerState{}, nil
}

func (s *stubBackend) Launch(context.Context, herder.Plan, herder.Unit) (herder.Instance, error) {
	s.launches++
	return nil, errNoContainers
}

var errNoContainers = errString("this test backend creates no containers")

type errString string

func (e errString) Error() string { return string(e) }

type cli struct {
	args    []string
	stdin   string
	backend herder.Backend
	version string
	image   string
}

type cliResult struct {
	exit   int
	stdout string
	stderr string
}

func (c cli) run(t *testing.T) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	backend := c.backend
	if backend == nil {
		backend = &stubBackend{}
	}
	version := c.version
	if version == "" {
		version = "dev"
	}
	exit := run(env{
		args:    c.args,
		stdin:   strings.NewReader(c.stdin),
		stdout:  &stdout,
		stderr:  &stderr,
		getenv:  func(string) string { return "" },
		version: version,
		image:   c.image,
		backend: func(context.Context) (herder.Backend, error) { return backend, nil },
		signals: make(chan os.Signal),
	})
	return cliResult{exit: exit, stdout: stdout.String(), stderr: stderr.String()}
}

func TestVersionPrintsPlainTextAndEntersNoProtocol(t *testing.T) {
	got := cli{args: []string{"--version"}, version: "v0.5.0"}.run(t)
	if got.exit != 0 {
		t.Fatalf("exit = %d, want 0", got.exit)
	}
	if got.stdout != "v0.5.0\n" {
		t.Fatalf("stdout = %q, want the plain version", got.stdout)
	}
	if strings.Contains(got.stdout, "protocol") {
		t.Fatal("--version entered the NDJSON protocol")
	}
}

func TestMissingRequiredFlagsAreUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"no network": {"--inform-url", "http://172.28.0.2:8080/inform", "--devices", "-"},
		"no inform":  {"--network", "net", "--devices", "-"},
		"no devices": {"--network", "net", "--inform-url", "http://172.28.0.2:8080/inform"},
		"nothing":    {},
		"unknown flag": {"--network", "net", "--inform-url", "http://172.28.0.2:8080/inform",
			"--devices", "-", "--privileged"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			got := cli{args: args}.run(t)
			if got.exit != 2 {
				t.Fatalf("exit = %d, want 2", got.exit)
			}
			if got.stdout != "" {
				t.Fatalf("stdout = %q, want no NDJSON before protocol mode", got.stdout)
			}
			if got.stderr == "" {
				t.Fatal("no diagnostic on stderr")
			}
		})
	}
}

// Positional arguments are the shape of a mistyped flag, and the command has
// no subcommands, so they are usage errors rather than silently ignored.
func TestPositionalArgumentsAreUsageErrors(t *testing.T) {
	got := cli{args: []string{"--network", "net", "--inform-url", "http://172.28.0.2:8080/inform",
		"--devices", "-", "start"}}.run(t)
	if got.exit != 2 {
		t.Fatalf("exit = %d, want 2", got.exit)
	}
}

func TestRequestErrorsHappenAfterStartedAndExitOne(t *testing.T) {
	got := cli{
		args:  []string{"--network", "net", "--inform-url", "http://172.28.0.2:8080/inform", "--devices", "-"},
		stdin: `{"version":1,"devices":[{"model":"USM8P","ip":"1.2.3.4"}]}`,
	}.run(t)
	if got.exit != 1 {
		t.Fatalf("exit = %d, want 1", got.exit)
	}
	events := decodeNDJSON(t, got.stdout)
	if len(events) != 2 || events[0]["event"] != "started" {
		t.Fatalf("events = %#v, want started then failed", events)
	}
	if events[1]["code"] != "invalid_request" {
		t.Fatalf("code = %v, want invalid_request", events[1]["code"])
	}
}

// A missing --devices file is a request problem, not CLI usage: the run has
// already entered protocol mode, so it reports invalid_request and exits 1.
func TestMissingDevicesFileIsARequestFailure(t *testing.T) {
	got := cli{args: []string{"--network", "net", "--inform-url", "http://172.28.0.2:8080/inform",
		"--devices", filepath.Join(t.TempDir(), "absent.json")}}.run(t)
	if got.exit != 1 {
		t.Fatalf("exit = %d, want 1", got.exit)
	}
	events := decodeNDJSON(t, got.stdout)
	if events[len(events)-1]["code"] != "invalid_request" {
		t.Fatalf("terminal = %#v, want invalid_request", events[len(events)-1])
	}
}

func TestDevicesFileIsRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"devices":[{"model":"USM8P"}]}`), 0o600); err != nil {
		t.Fatalf("write devices: %v", err)
	}
	backend := &stubBackend{}
	got := cli{
		args: []string{"--network", "net", "--inform-url", "http://172.28.0.2:8080/inform",
			"--devices", path, "--synthetic-image", "public/emu:1.0.0"},
		backend: backend,
	}.run(t)
	if backend.launches == 0 {
		t.Fatalf("the request never reached the start phase (exit %d, stdout %s)", got.exit, got.stdout)
	}
}

// A development binary has no compiled image default, so a synthetic device
// needs --synthetic-image and says so rather than reaching for a floating tag.
func TestDevelopmentBuildRequiresASyntheticImage(t *testing.T) {
	got := cli{
		args:  []string{"--network", "net", "--inform-url", "http://172.28.0.2:8080/inform", "--devices", "-"},
		stdin: `{"version":1,"devices":[{"model":"USM8P"}]}`,
	}.run(t)
	events := decodeNDJSON(t, got.stdout)
	terminal := events[len(events)-1]
	if terminal["code"] != "synthetic_image_required" {
		t.Fatalf("terminal = %#v, want synthetic_image_required", terminal)
	}
	if got.exit != 1 {
		t.Fatalf("exit = %d, want 1", got.exit)
	}
}

func TestReleaseBuildUsesItsVersionMatchedImage(t *testing.T) {
	backend := &stubBackend{}
	got := cli{
		args:    []string{"--network", "net", "--inform-url", "http://172.28.0.2:8080/inform", "--devices", "-"},
		stdin:   `{"version":1,"devices":[{"model":"USM8P"}]}`,
		version: "v0.5.0",
		image:   herder.DefaultSyntheticImage("v0.5.0"),
		backend: backend,
	}.run(t)
	if backend.launches == 0 {
		t.Fatalf("the compiled image default was not used (stdout %s stderr %s)", got.stdout, got.stderr)
	}
}

func TestEveryStdoutLineIsOneProtocolEvent(t *testing.T) {
	got := cli{
		args:  []string{"--network", "net", "--inform-url", "http://172.28.0.2:8080/inform", "--devices", "-"},
		stdin: `{"version":1,"devices":[{"model":"USM8P"}]}`,
	}.run(t)
	for _, line := range strings.Split(strings.TrimSpace(got.stdout), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout line %q is not one JSON object: %v", line, err)
		}
		if ev["protocol"] == nil || ev["run_id"] == nil {
			t.Fatalf("event %q lacks protocol or run_id", line)
		}
	}
}

func TestTimeoutFlagsHaveTheDocumentedDefaults(t *testing.T) {
	flags, _, err := parseFlags([]string{
		"--network", "net", "--inform-url", "http://172.28.0.2:8080/inform", "--devices", "-",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if flags.startupTimeout != 5*time.Minute {
		t.Fatalf("startup timeout = %s, want 5m", flags.startupTimeout)
	}
	if flags.stopTimeout != 30*time.Second {
		t.Fatalf("stop timeout = %s, want 30s", flags.stopTimeout)
	}
}

func decodeNDJSON(t *testing.T, body string) []map[string]any {
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
