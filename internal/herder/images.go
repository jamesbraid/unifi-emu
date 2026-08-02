package herder

import (
	"context"
	"errors"
	"strings"

	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
)

// minAPIVersion is the Docker API that added endpoint MAC assignment on an
// existing network. Older daemons silently ignore the field, which would
// produce a fleet whose endpoint MACs do not match the identities the herder
// published.
//
// The API version is the whole gate. Engine 25 is only the release that
// introduced 1.44, and asking the engine as well would accept a daemon capped
// to a lower API by configuration -- which then fails at container creation,
// the confusing failure this preflight exists to prevent.
const minAPIVersion = "1.44"

// DockerAPI is the slice of the official Moby client the herder uses. It
// covers the all-images-before-create preflight and the inspection loop only:
// container creation, health waits, logs and termination stay with stock
// testcontainers-go, and this preflight does not replace any of that.
type DockerAPI interface {
	ServerVersion(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error)
	NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error)
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// CheckDocker confirms the daemon answers and is new enough.
func CheckDocker(ctx context.Context, api DockerAPI) error {
	version, err := api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return wrapf(err, CodeDockerUnavailable, PhaseValidate, "server version: %v", err)
	}
	return checkDockerVersion(version)
}

func checkDockerVersion(v client.ServerVersionResult) error {
	if !versions.GreaterThanOrEqualTo(v.APIVersion, minAPIVersion) {
		return failf(CodeDockerVersionUnsupported, PhaseValidate,
			"Docker API %q is older than %s", v.APIVersion, minAPIVersion)
	}
	return nil
}

// CheckNetwork confirms the caller-supplied network exists. The herder never
// creates or removes it: the network belongs to the downstream harness, and
// so does the controller sitting on it.
func CheckNetwork(ctx context.Context, api DockerAPI, name string) error {
	if _, err := api.NetworkInspect(ctx, name, client.NetworkInspectOptions{}); err != nil {
		return wrapf(err, CodeNetworkNotFound, PhaseValidate, "inspect network %s: %v", name, err)
	}
	return nil
}

// PullAndInspect pulls every image the plan needs and checks each against the
// device-container contract, all before the first container is created. A
// registry failure or an image that cannot satisfy the contract therefore
// leaves no half-started fleet behind.
func PullAndInspect(ctx context.Context, api DockerAPI, plan Plan) error {
	for _, ref := range plan.Images() {
		if err := pull(ctx, api, ref); err != nil {
			return err
		}
	}
	for _, unit := range plan.Units {
		inspected, err := api.ImageInspect(ctx, unit.Image)
		if err != nil {
			return wrapf(err, CodeImageInvalid, PhasePull, "inspect image %s: %v", unit.Image, err)
		}
		if err := checkImage(unit, inspected.InspectResponse); err != nil {
			return err
		}
	}
	return nil
}

// pull fetches one reference. A reference the runner built itself -- the
// development synthetic image, a locally built fixture -- exists in no
// registry, so its pull always fails while the image is right there. That is
// not a missing image, so the failure falls back to the local copy; only an
// image that is neither pullable nor present is image_pull_failed.
func pull(ctx context.Context, api DockerAPI, ref string) error {
	pullErr := func() error {
		resp, err := api.ImagePull(ctx, ref, client.ImagePullOptions{})
		if err != nil {
			return err
		}
		defer resp.Close()
		return resp.Wait(ctx)
	}()
	if pullErr == nil {
		return nil
	}
	// The startup deadline outranks everything: a registry stall must be
	// reported as the timeout it is, not as a missing image.
	if errors.Is(pullErr, context.DeadlineExceeded) || errors.Is(pullErr, context.Canceled) {
		return wrapf(pullErr, CodeStartupTimeout, PhasePull, "pull %s: %v", ref, pullErr)
	}
	if _, err := api.ImageInspect(ctx, ref); err != nil {
		return wrapf(pullErr, CodeImagePullFailed, PhasePull, "pull %s: %v", ref, pullErr)
	}
	return nil
}

// checkImage enforces the parts of the container contract that are visible in
// image metadata: a declared HEALTHCHECK -- the herder's only readiness
// signal, since it never parses container logs -- and no EXPOSE entries,
// because the herder publishes nothing to the host. Opaque runtime images are
// additionally pinned to linux/amd64; the public synthetic image may be
// multi-architecture as long as the daemon resolves it to amd64.
func checkImage(unit Unit, got image.InspectResponse) error {
	if got.Config == nil || got.Config.Healthcheck == nil ||
		len(got.Config.Healthcheck.Test) == 0 || got.Config.Healthcheck.Test[0] == "NONE" {
		return failf(CodeImageInvalid, PhasePull, "image %s declares no HEALTHCHECK", unit.Image)
	}
	if len(got.Config.ExposedPorts) > 0 {
		return failf(CodeImageInvalid, PhasePull, "image %s declares EXPOSE ports", unit.Image)
	}
	if unit.Kind == UnitOpaque && (got.Os != "linux" || got.Architecture != "amd64") {
		return failf(CodeImageInvalid, PhasePull,
			"image %s is %s/%s, want linux/amd64", unit.Image, got.Os, got.Architecture)
	}
	return nil
}

// redactor replaces every configured opaque runtime image reference and its
// registry host before a Docker error reaches stderr. Public control events
// never carry these strings at all; this is the belt for the diagnostics that
// operators still need to debug a runtime that will not start.
type redactor struct{ terms []string }

const redactedRuntime = "[opaque-runtime]"

func newRedactor(cfg RuntimeConfig) *redactor {
	r := &redactor{}
	seen := map[string]bool{}
	for _, rt := range cfg.Models {
		for _, term := range []string{rt.Image, registryHost(rt.Image)} {
			if term == "" || seen[term] {
				continue
			}
			seen[term] = true
			r.terms = append(r.terms, term)
		}
	}
	// Longest first, so a full reference is replaced before its registry
	// host matches part of it and leaves the rest exposed.
	for i := 1; i < len(r.terms); i++ {
		for j := i; j > 0 && len(r.terms[j]) > len(r.terms[j-1]); j-- {
			r.terms[j], r.terms[j-1] = r.terms[j-1], r.terms[j]
		}
	}
	return r
}

func (r *redactor) redact(s string) string {
	for _, term := range r.terms {
		s = strings.ReplaceAll(s, term, redactedRuntime)
	}
	return s
}

// registryHost is the reference's leading host component, when it has one.
func registryHost(ref string) string {
	host, _, ok := strings.Cut(ref, "/")
	if !ok || !strings.ContainsAny(host, ".:") {
		return "" // a bare repository name has no registry host
	}
	return host
}
