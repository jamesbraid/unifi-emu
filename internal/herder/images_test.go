package herder

import (
	"context"
	"errors"
	"io"
	"iter"
	"reflect"
	"strings"
	"testing"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/client"
)

// fakeDocker is the narrow Docker surface the preflight uses, scripted.
type fakeDocker struct {
	version     client.ServerVersionResult
	versionErr  error
	networks    map[string]bool
	networkErr  error
	images      map[string]image.InspectResponse
	inspectErr  error
	pullErr     error
	pulled      []string
	inspected   []string
	networkSeen []string
	// containers is the daemon's inventory. ContainerList applies the caller's
	// filters to it, so a sweep that filters too loosely deletes a decoy and
	// the test sees it.
	containers []fakeContainer
	removed    []string // container ids ContainerRemove was called with
	// requireLiveContext makes the fake behave like the real client, which
	// fails immediately once its context is done.
	requireLiveContext bool
}

func (f *fakeDocker) ServerVersion(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error) {
	return f.version, f.versionErr
}

func (f *fakeDocker) NetworkInspect(_ context.Context, name string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	f.networkSeen = append(f.networkSeen, name)
	if f.networkErr != nil {
		return client.NetworkInspectResult{}, f.networkErr
	}
	if !f.networks[name] {
		return client.NetworkInspectResult{}, errNoSuchNetwork
	}
	return client.NetworkInspectResult{}, nil
}

func (f *fakeDocker) ImagePull(_ context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.pulled = append(f.pulled, ref)
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return fakePull{}, nil
}

func (f *fakeDocker) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.inspected = append(f.inspected, ref)
	if f.inspectErr != nil {
		return client.ImageInspectResult{}, f.inspectErr
	}
	got, ok := f.images[ref]
	if !ok {
		return client.ImageInspectResult{}, errors.New("no such image")
	}
	return client.ImageInspectResult{InspectResponse: got}, nil
}

func (f *fakeDocker) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{}, errors.New("not used in this test")
}

type fakeContainer struct {
	id     string
	labels map[string]string
	status string
}

func (f *fakeDocker) ContainerList(ctx context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error) {
	if f.requireLiveContext && ctx.Err() != nil {
		return client.ContainerListResult{}, ctx.Err()
	}
	out := client.ContainerListResult{}
	for _, c := range f.containers {
		if matchesFilters(c, opts.Filters) {
			out.Items = append(out.Items, container.Summary{ID: c.id})
		}
	}
	return out, nil
}

// matchesFilters implements the label and status terms the herder uses, the
// way the daemon does: every term must match.
func matchesFilters(c fakeContainer, filters client.Filters) bool {
	for term, values := range filters {
		for value := range values {
			switch term {
			case "label":
				key, want, found := strings.Cut(value, "=")
				if !found || c.labels[key] != want {
					return false
				}
			case "status":
				if c.status != value {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

func (f *fakeDocker) ContainerRemove(ctx context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if f.requireLiveContext && ctx.Err() != nil {
		return client.ContainerRemoveResult{}, ctx.Err()
	}
	f.removed = append(f.removed, id)
	return client.ContainerRemoveResult{}, nil
}

var errNoSuchNetwork = errors.New("network not found")

type fakePull struct{}

func (fakePull) Read([]byte) (int, error) { return 0, io.EOF }
func (fakePull) Close() error             { return nil }
func (fakePull) Wait(context.Context) error {
	return nil
}

func (fakePull) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(func(jsonstream.Message, error) bool) {}
}

func healthyImage() image.InspectResponse {
	return image.InspectResponse{
		Architecture: "amd64",
		Os:           "linux",
		Config: &dockerspec.DockerOCIImageConfig{
			DockerOCIImageConfigExt: dockerspec.DockerOCIImageConfigExt{
				Healthcheck: &dockerspec.HealthcheckConfig{Test: []string{"CMD", "/probe"}},
			},
		},
	}
}

// The gate is the API version. API 1.44 is what endpoint MAC assignment on an
// existing network needs; engine 25 is merely the release that introduced it.
func TestDockerAPIVersionFloor(t *testing.T) {
	cases := map[string]bool{
		"1.44": true, "1.51": true, "1.54": true, "2.0": true,
		"1.43": false, "0.99": false, "": false, "nonsense": false,
	}
	for api, ok := range cases {
		err := checkDockerVersion(client.ServerVersionResult{Version: "25.0.0", APIVersion: api})
		if ok && err != nil {
			t.Errorf("API %q rejected: %v", api, err)
		}
		if !ok {
			if err == nil {
				t.Errorf("API %q accepted, want a rejection", api)
				continue
			}
			assertFailure(t, err, CodeDockerVersionUnsupported, PhaseValidate)
		}
	}
}

// The engine version is not consulted at all. A daemon capped to a lower API
// through configuration would pass an engine check and then fail at container
// creation, which is the confusing failure this preflight exists to prevent --
// so the API version is the only thing asked.
func TestEngineVersionIsNotConsulted(t *testing.T) {
	for _, engine := range []string{"24.0.7", "", "nonsense", "99.0.0", "25.0.3-ce"} {
		if err := checkDockerVersion(client.ServerVersionResult{
			Version: engine, APIVersion: "1.44",
		}); err != nil {
			t.Errorf("engine %q rejected while the API is 1.44: %v", engine, err)
		}
	}
}

func TestUnreachableDaemonIsDockerUnavailable(t *testing.T) {
	api := &fakeDocker{versionErr: errors.New("cannot connect to the Docker daemon")}
	err := CheckDocker(context.Background(), api)
	assertFailure(t, err, CodeDockerUnavailable, PhaseValidate)
}

func TestMissingNetworkIsNetworkNotFound(t *testing.T) {
	api := &fakeDocker{networks: map[string]bool{"other": true}}
	err := CheckNetwork(context.Background(), api, "unifi-lab")
	assertFailure(t, err, CodeNetworkNotFound, PhaseValidate)
}

func TestExistingNetworkPasses(t *testing.T) {
	api := &fakeDocker{networks: map[string]bool{"unifi-lab": true}}
	if err := CheckNetwork(context.Background(), api, "unifi-lab"); err != nil {
		t.Fatalf("CheckNetwork: %v", err)
	}
}

func TestPullsEveryImageOnceBeforeInspection(t *testing.T) {
	plan := buildPlan(t, ids("USM8P", "UXGENT", "UXGENT"), mappedConfig("UXGENT"), "public/emu:1.0.0")
	api := &fakeDocker{images: map[string]image.InspectResponse{}}
	for _, ref := range plan.Images() {
		api.images[ref] = healthyImage()
	}
	if err := PullAndInspect(context.Background(), api, plan); err != nil {
		t.Fatalf("PullAndInspect: %v", err)
	}
	if !reflect.DeepEqual(api.pulled, plan.Images()) {
		t.Fatalf("pulled %#v, want each image once in plan order %#v", api.pulled, plan.Images())
	}
}

func TestPullFailureIsImagePullFailed(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	api := &fakeDocker{pullErr: errors.New("manifest unknown")}
	err := PullAndInspect(context.Background(), api, plan)
	assertFailure(t, err, CodeImagePullFailed, PhasePull)
}

func TestImageWithoutHealthcheckIsInvalid(t *testing.T) {
	img := healthyImage()
	img.Config.Healthcheck = nil
	assertImageRejected(t, img)
}

func TestImageWithDisabledHealthcheckIsInvalid(t *testing.T) {
	img := healthyImage()
	img.Config.Healthcheck = &dockerspec.HealthcheckConfig{Test: []string{"NONE"}}
	assertImageRejected(t, img)
}

func TestImageWithEmptyHealthcheckTestIsInvalid(t *testing.T) {
	img := healthyImage()
	img.Config.Healthcheck = &dockerspec.HealthcheckConfig{}
	assertImageRejected(t, img)
}

func TestImageWithExposedPortsIsInvalid(t *testing.T) {
	img := healthyImage()
	img.Config.ExposedPorts = map[string]struct{}{"8080/tcp": {}}
	assertImageRejected(t, img)
}

func assertImageRejected(t *testing.T, img image.InspectResponse) {
	t.Helper()
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	api := &fakeDocker{images: map[string]image.InspectResponse{"public/emu:1.0.0": img}}
	err := PullAndInspect(context.Background(), api, plan)
	assertFailure(t, err, CodeImageInvalid, PhasePull)
}

// An opaque runtime mapping is always a digest-pinned linux/amd64 image. The
// public synthetic image may be multi-architecture, so only the opaque side
// is pinned to a platform here.
func TestOpaqueImageMustBeLinuxAMD64(t *testing.T) {
	plan := buildPlan(t, ids("UXGENT"), mappedConfig("UXGENT"), "")
	ref := plan.Images()[0]
	for _, bad := range []image.InspectResponse{
		{Architecture: "arm64", Os: "linux", Config: healthyImage().Config},
		{Architecture: "amd64", Os: "windows", Config: healthyImage().Config},
	} {
		api := &fakeDocker{images: map[string]image.InspectResponse{ref: bad}}
		err := PullAndInspect(context.Background(), api, plan)
		assertFailure(t, err, CodeImageInvalid, PhasePull)
	}
}

func TestInspectFailureIsImageInvalid(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	api := &fakeDocker{inspectErr: errors.New("no such image")}
	err := PullAndInspect(context.Background(), api, plan)
	assertFailure(t, err, CodeImageInvalid, PhasePull)
}

// The stderr diagnostic for a pull failure must not carry the registry host
// or the image reference: those are the runner's business, not the public
// job's, and the redactor is what keeps them out.
func TestRedactorHidesConfiguredImagesAndRegistries(t *testing.T) {
	cfg := RuntimeConfig{Version: 1, Models: map[string]ModelRuntime{
		"UXGENT": {Image: "forge.example/emu/uxgent@" + testDigest},
	}}
	r := newRedactor(cfg)
	got := r.redact("pull forge.example/emu/uxgent@" + testDigest + " from forge.example failed")
	if strings.Contains(got, "forge.example") || strings.Contains(got, testDigest) {
		t.Fatalf("redacted text still names the runtime: %q", got)
	}
	if !strings.Contains(got, "[opaque-runtime]") {
		t.Fatalf("redacted text = %q, want the placeholder", got)
	}
}

func TestRedactorLeavesPublicTextAlone(t *testing.T) {
	r := newRedactor(RuntimeConfig{})
	const msg = "pull ghcr.io/jamesbraid/unifi-emu:1.0.0 failed"
	if got := r.redact(msg); got != msg {
		t.Fatalf("redacted public text: %q", got)
	}
}

// A development or CI image built from this checkout exists only on the
// runner, so its pull always fails. That is not a missing image: the preflight
// falls back to the local copy when one is present, which is what makes
// --synthetic-image and a locally built fixture usable at all.
func TestPullFallsBackToAnImageAlreadyPresentLocally(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "local/emu:dev")
	api := &fakeDocker{
		pullErr: errors.New("pull access denied for local/emu"),
		images:  map[string]image.InspectResponse{"local/emu:dev": healthyImage()},
	}
	if err := PullAndInspect(context.Background(), api, plan); err != nil {
		t.Fatalf("PullAndInspect: %v", err)
	}
}

func TestPullFailureForAnAbsentImageStillFails(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "absent/emu:dev")
	api := &fakeDocker{
		pullErr:    errors.New("manifest unknown"),
		inspectErr: errors.New("no such image"),
	}
	err := PullAndInspect(context.Background(), api, plan)
	assertFailure(t, err, CodeImagePullFailed, PhasePull)
}

// The capability precheck rejects only a daemon class that cannot apply Linux
// capabilities at all. Everything else stays Docker's decision, reported as
// container_start_failed in phase start when creation refuses it.
func TestCapabilityPrecheckRejectsANonLinuxDaemon(t *testing.T) {
	cfg := RuntimeConfig{Version: 1, Models: map[string]ModelRuntime{
		"UXGENT": {Image: "fixture.example/x@" + testDigest, CapAdd: []string{"NET_ADMIN"}},
	}}
	plan := buildPlan(t, ids("UXGENT"), cfg, "")
	backend := &dockerBackend{api: &fakeDocker{
		version: client.ServerVersionResult{Version: "25.0.0", APIVersion: "1.44", Os: "windows"},
	}}
	err := backend.CheckCapabilities(context.Background(), plan)
	assertFailure(t, err, CodeCapabilityUnsupported, PhaseValidate)
}

func TestCapabilityPrecheckAcceptsALinuxDaemon(t *testing.T) {
	cfg := RuntimeConfig{Version: 1, Models: map[string]ModelRuntime{
		"UXGENT": {Image: "fixture.example/x@" + testDigest, CapAdd: []string{"SYS_ADMIN"}},
	}}
	plan := buildPlan(t, ids("UXGENT"), cfg, "")
	backend := &dockerBackend{api: &fakeDocker{
		version: client.ServerVersionResult{Version: "25.0.0", APIVersion: "1.44", Os: "linux"},
	}}
	if err := backend.CheckCapabilities(context.Background(), plan); err != nil {
		t.Fatalf("CheckCapabilities: %v", err)
	}
}

// A plan that adds no capability must not even ask the daemon: an all-synthetic
// run has nothing for the precheck to decide.
func TestCapabilityPrecheckSkipsAPlanWithNoCapabilities(t *testing.T) {
	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	api := &fakeDocker{versionErr: errors.New("the daemon must not be consulted")}
	backend := &dockerBackend{api: api}
	if err := backend.CheckCapabilities(context.Background(), plan); err != nil {
		t.Fatalf("CheckCapabilities: %v", err)
	}
}
