package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

type Harness struct {
	Ctx          context.Context
	Cancel       context.CancelFunc
	Network      *testcontainers.DockerNetwork
	Controller   testcontainers.Container
	ApiURL       string
	ApiPort      string
	ControllerIP string
	Runtimes     []testcontainers.Container
	Evidence     string
	Plan         *LaunchPlan
	LogNames     []string
	Config       HarnessConfig
}

type HarnessConfig struct {
	TestName        string
	Timeout         time.Duration
	ControllerImage string
	ControllerPort  string // "8443" or "443"
	EmulatorImage   string // if prebuilt, or empty to build from checkout
	CatalogPath     string // UNIFI_EMU_RUNTIME_CATALOG path
	MacBase         string
	IpBase          string
	ExtraEnv        map[string]string // env variables for the synthetic container (e.g. SIM_ADOPT)
	IsUOS           bool
	ContextDir      string
	GoArch          string
}

func applyUOSHostConfig(cfg *container.HostConfig) {
	cfg.CgroupnsMode = container.CgroupnsModeHost
	cfg.Binds = []string{"/sys/fs/cgroup:/sys/fs/cgroup:rw"}
	cfg.CapDrop = []string{"ALL"}
	cfg.CapAdd = []string{
		"SYS_ADMIN", "NET_ADMIN", "NET_RAW", "NET_BIND_SERVICE",
		"DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "CHOWN",
		"SETUID", "SETGID", "KILL", "SYS_CHROOT", "SYS_PTRACE",
		"SYS_RESOURCE", "AUDIT_WRITE", "MKNOD",
	}
	cfg.Tmpfs = map[string]string{
		"/run":               "exec",
		"/run/lock":          "",
		"/tmp":               "exec",
		"/var/lib/journal":   "",
		"/var/opt/unifi/tmp": "size=64m",
	}
}

func BuildControllerRequest(networkName, image string, isUOS bool) testcontainers.ContainerRequest {
	if isUOS {
		return testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"443/tcp"},
			Networks:     []string{networkName},
			NetworkAliases: map[string][]string{
				networkName: {"controller"},
			},
			WaitingFor: wait.ForHealthCheck().
				WithPollInterval(time.Second).
				WithStartupTimeout(10 * time.Minute),
			HostConfigModifier: applyUOSHostConfig,
		}
	}
	return testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{"8443/tcp"},
		Networks:     []string{networkName},
		NetworkAliases: map[string][]string{
			networkName: {"controller"},
		},
		WaitingFor: wait.ForHealthCheck().
			WithPollInterval(time.Second).
			WithStartupTimeout(5 * time.Minute),
	}
}

func ConfigureContainerRuntime() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	host := DiscoverDockerHost(os.Getenv("DOCKER_HOST"), home, func(path string) bool {
		info, statErr := os.Stat(path)
		return statErr == nil && info.Mode()&os.ModeSocket != 0
	})
	if host != "" && os.Getenv("DOCKER_HOST") == "" {
		if err := os.Setenv("DOCKER_HOST", host); err != nil {
			return fmt.Errorf("select container runtime %s: %w", host, err)
		}
	}
	if override := ContainerRuntimeSocketOverride(host); override != "" &&
		os.Getenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE") == "" {
		if err := os.Setenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", override); err != nil {
			return fmt.Errorf("configure runtime socket override %s: %w", override, err)
		}
	}
	return nil
}

func DiscoverDockerHost(current, home string, socketExists func(string) bool) string {
	if current != "" {
		return current
	}
	colima := filepath.Join(home, ".colima", "default", "docker.sock")
	if socketExists(colima) {
		return "unix://" + colima
	}
	return ""
}

func ContainerRuntimeSocketOverride(host string) string {
	if strings.Contains(host, "/.colima/") {
		return "/var/run/docker.sock"
	}
	return ""
}

func NormalizeTestName(testName string) string {
	var normalized strings.Builder
	underscore := false
	for _, r := range strings.ToLower(testName) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			normalized.WriteRune(r)
			underscore = false
			continue
		}
		if !underscore && normalized.Len() > 0 {
			normalized.WriteByte('_')
			underscore = true
		}
	}
	return strings.Trim(normalized.String(), "_")
}

func EmulatorBuildArgs(goarch string) map[string]*string {
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	buildPlatform := "linux/" + goarch
	targetOS := "linux"
	targetArch := goarch
	return map[string]*string{
		"BUILDPLATFORM": &buildPlatform,
		"TARGETOS":      &targetOS,
		"TARGETARCH":    &targetArch,
	}
}

func NewHarness(cfg HarnessConfig) (*Harness, error) {
	if err := ConfigureContainerRuntime(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	evidence := filepath.Join("tmp", "itest", NormalizeTestName(cfg.TestName))
	if err := os.RemoveAll(evidence); err != nil {
		cancel()
		return nil, fmt.Errorf("clear evidence directory %s: %w", evidence, err)
	}
	if err := os.MkdirAll(evidence, 0o755); err != nil {
		cancel()
		return nil, fmt.Errorf("create evidence directory %s: %w", evidence, err)
	}

	h := &Harness{
		Ctx:      ctx,
		Cancel:   cancel,
		Evidence: evidence,
		Config:   cfg,
	}

	// Start network
	network, err := tcnetwork.New(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create Testcontainers network: %w", err)
	}
	h.Network = network

	// Start controller
	controllerReq := BuildControllerRequest(network.Name, cfg.ControllerImage, cfg.IsUOS)
	controller, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: controllerReq,
		Started:          true,
	})
	if controller != nil {
		h.Controller = controller
	}
	if err != nil {
		h.closeNoLogs()
		return nil, fmt.Errorf("start controller container: %w", err)
	}

	h.ControllerIP, err = controller.ContainerIP(ctx)
	if err != nil {
		h.closeNoLogs()
		return nil, fmt.Errorf("resolve controller IP: %w", err)
	}
	if h.ControllerIP == "" {
		h.closeNoLogs()
		return nil, errors.New("resolve controller IP: empty address returned")
	}

	mappedHost, err := controller.Host(ctx)
	if err != nil {
		h.closeNoLogs()
		return nil, fmt.Errorf("resolve controller Host: %w", err)
	}
	mappedPort, err := controller.MappedPort(ctx, cfg.ControllerPort+"/tcp")
	if err != nil {
		h.closeNoLogs()
		return nil, fmt.Errorf("resolve controller MappedPort: %w", err)
	}
	h.ApiURL = "https://" + mappedHost + ":" + mappedPort.Port()
	h.ApiPort = cfg.ControllerPort

	return h, nil
}

func (h *Harness) StartRuntimes(specs []emu.DeviceSpec) error {
	// 1. Load catalog & validate launch plan first
	cat, err := LoadCatalog(h.Config.CatalogPath)
	if err != nil {
		return fmt.Errorf("catalog load error: %w", err)
	}

	expandedSpecs, err := ExpandFleet(specs, cat, h.Config.MacBase, h.Config.IpBase)
	if err != nil {
		return fmt.Errorf("expand fleet error: %w", err)
	}

	plan, err := PlanLaunch(expandedSpecs, cat)
	if err != nil {
		return fmt.Errorf("plan launch error: %w", err)
	}
	h.Plan = plan

	informURL := "http://" + h.ControllerIP + ":8080/inform"

	// Start runtimes one by one
	for i, cp := range plan.Containers {
		req, err := BuildContainerRequest(cp, h.Config, h.Network.Name, informURL)
		if err != nil {
			return fmt.Errorf("build runtime container request %d (%s): %w", i, cp.RuntimeName, err)
		}

		c, startErr := testcontainers.GenericContainer(h.Ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		if startErr != nil {
			if c != nil {
				h.Runtimes = append(h.Runtimes, c)
				h.LogNames = append(h.LogNames, cp.LogName)
			}
			return fmt.Errorf("start runtime container %d (%s): %w", i, cp.RuntimeName, startErr)
		}

		h.Runtimes = append(h.Runtimes, c)
		h.LogNames = append(h.LogNames, cp.LogName)
	}

	return nil
}

func BuildContainerRequest(cp ContainerPlan, cfg HarnessConfig, networkName, informURL string) (testcontainers.ContainerRequest, error) {
	encodedJSON, err := json.Marshal(cp.Specs)
	if err != nil {
		return testcontainers.ContainerRequest{}, err
	}

	hasLegacy := false
	if cp.RuntimeName == "synthetic" {
		if cfg.ExtraEnv["SIM_MODELS"] != "" || cfg.ExtraEnv["SIM_DEVICES"] != "" {
			hasLegacy = true
		}
	}

	env := map[string]string{
		"UNIFI_EMU_INFORM_URL": informURL,
		"SIM_CONTROLLER":       informURL,
	}
	for k, v := range cfg.ExtraEnv {
		env[k] = v
	}

	isFlagExpress := !hasLegacy && cp.RuntimeName == "synthetic" && len(cp.Specs) == 1 && flagsExpress(cp.Specs[0])
	if !hasLegacy && !isFlagExpress {
		env["UNIFI_EMU_DEVICES_JSON"] = string(encodedJSON)
	}

	req := testcontainers.ContainerRequest{
		Networks: []string{networkName},
		Env:      env,
	}

	if cp.RuntimeName == "synthetic" {
		if cfg.EmulatorImage == "" {
			contextDir := cfg.ContextDir
			if contextDir == "" {
				contextDir = "."
			}
			req.FromDockerfile = testcontainers.FromDockerfile{
				Context:    contextDir,
				Dockerfile: "Dockerfile",
				Repo:       "unifi-emu-itest",
				Tag:        strings.ReplaceAll(networkName, "_", "-"),
				BuildArgs:  EmulatorBuildArgs(cfg.GoArch),
			}
		} else {
			req.Image = cfg.EmulatorImage
		}

		// Replicate legacy Cmd setup for backward compatibility
		if !hasLegacy && len(cp.Specs) == 1 && flagsExpress(cp.Specs[0]) {
			spec := cp.Specs[0]
			cmd := []string{"-inform", informURL, "-mac", spec.MAC, "-model", spec.Model, "-ip", spec.IP}
			if spec.Type != "" {
				cmd = append(cmd, "-type", spec.Type)
			}
			if spec.ModelDisplay != "" {
				cmd = append(cmd, "-model-display", spec.ModelDisplay)
			}
			if spec.Version != "" {
				cmd = append(cmd, "-version", spec.Version)
			}
			if spec.Name != "" {
				cmd = append(cmd, "-name", spec.Name)
			}
			req.Cmd = cmd
		} else if !hasLegacy {
			req.Cmd = []string{"-inform", informURL}
		}
	} else {
		req.Image = cp.Image
		if len(cp.CapAdd) > 0 {
			req.HostConfigModifier = func(cfgModifier *container.HostConfig) {
				cfgModifier.CapAdd = append(cfgModifier.CapAdd, cp.CapAdd...)
			}
		}
		// Wait strategy for OCI runtimes (except public fixture)
		if cp.Image != "public-fixture:local" {
			req.WaitingFor = wait.ForHealthCheck().
				WithPollInterval(time.Second).
				WithStartupTimeout(2 * time.Minute)
		}
	}

	return req, nil
}

func (h *Harness) closeNoLogs() {
	captureCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if h.Controller != nil {
		h.Controller.Terminate(captureCtx)
	}
	if h.Network != nil {
		h.Network.Remove(captureCtx)
	}
	h.Cancel()
}

func (h *Harness) Close() {
	captureCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	h.captureLogs(captureCtx, h.Controller, "controller.log")
	h.captureServerLog(captureCtx)
	for i, c := range h.Runtimes {
		h.captureLogs(captureCtx, c, h.LogNames[i])
	}

	// Stop runtimes first
	for _, c := range h.Runtimes {
		c.Terminate(captureCtx)
	}

	if h.Controller != nil {
		h.Controller.Terminate(captureCtx)
	}

	if h.Network != nil {
		h.Network.Remove(captureCtx)
	}

	h.Cancel()
}

func (h *Harness) captureLogs(ctx context.Context, target testcontainers.Container, name string) {
	if target == nil {
		h.writeEvidenceError(name, errors.New("container was not started"))
		return
	}
	logCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	logs, err := target.Logs(logCtx)
	if err != nil {
		h.writeEvidenceError(name, err)
		return
	}
	defer logs.Close()
	h.writeEvidenceReader(name, logs)
}

func (h *Harness) captureServerLog(ctx context.Context) {
	if h.Controller == nil {
		h.writeEvidenceError("server.log", errors.New("controller container was not started"))
		return
	}
	var errs []error
	for _, source := range []string{
		"/usr/lib/unifi/logs/server.log",
		"/unifi/logs/server.log",
	} {
		reader, err := h.Controller.CopyFileFromContainer(ctx, source)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", source, err))
			continue
		}
		h.writeEvidenceReader("server.log", reader)
		reader.Close()
		return
	}
	h.writeEvidenceError("server.log", errors.Join(errs...))
}

func (h *Harness) writeEvidenceReader(name string, reader io.Reader) {
	path := filepath.Join(h.Evidence, name)
	file, err := os.Create(path)
	if err != nil {
		h.writeEvidenceError(name, err)
		return
	}
	defer file.Close()
	io.Copy(file, reader)
}

func (h *Harness) writeEvidenceError(name string, err error) {
	if err == nil {
		return
	}
	path := filepath.Join(h.Evidence, name+".error.txt")
	os.WriteFile(path, []byte(err.Error()+"\n"), 0o644)
}

func (h *Harness) ExecReady(ctx context.Context, phase string, command []string) error {
	code, output, err := h.Controller.Exec(ctx, command)
	if err != nil {
		return fmt.Errorf("%s exec: %w", phase, err)
	}
	body, readErr := io.ReadAll(output)
	if readErr != nil {
		return fmt.Errorf("%s output: %w", phase, readErr)
	}
	if code != 0 {
		return fmt.Errorf("%s exit %d: %s", phase, code, body)
	}
	return nil
}

func (h *Harness) RequireRuntimesRunning() error {
	for i, c := range h.Runtimes {
		state, err := c.State(h.Ctx)
		if err != nil {
			return fmt.Errorf("inspect runtime container %d: %w", i, err)
		}
		if !state.Running {
			return fmt.Errorf("runtime container %d stopped: status=%s exit=%d error=%s",
				i, state.Status, state.ExitCode, state.Error)
		}
	}
	return nil
}

func flagsExpress(spec emu.DeviceSpec) bool {
	return spec.FWCaps == nil && len(spec.SSIDs) == 0 && spec.Ports == 0
}
