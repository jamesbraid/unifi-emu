package herder

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// dockerBackend is the production Backend. It splits responsibility exactly
// where the design does: the official Moby client performs the
// all-images-before-create preflight and every inspection, and stock
// testcontainers-go owns creation, health waits, logs, termination and the
// Ryuk session. Nothing here customizes Testcontainers lifecycle behaviour.
type dockerBackend struct {
	api DockerAPI
}

// newContainer is the library's creation entry point, indirected so the
// path where creation fails after the container already exists can be tested.
var newContainer = testcontainers.GenericContainer

// healthWait is the readiness strategy the herder waits on. Docker health
// state is the only readiness signal; container logs are opaque byte streams
// here and are never parsed for progress.
func healthWait() wait.Strategy { return wait.ForHealthCheck() }

// NewDockerBackend connects to the Docker daemon the way Testcontainers does,
// so the preflight and the lifecycle always talk to the same host.
func NewDockerBackend(ctx context.Context) (Backend, error) {
	api, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return nil, wrapf(err, CodeDockerUnavailable, PhaseValidate, "connect to Docker: %v", err)
	}
	return &dockerBackend{api: api}, nil
}

func (b *dockerBackend) CheckDaemon(ctx context.Context) error {
	return CheckDocker(ctx, b.api)
}

func (b *dockerBackend) CheckNetwork(ctx context.Context, name string) error {
	return CheckNetwork(ctx, b.api, name)
}

// CheckCapabilities rejects only daemon classes known to be incompatible with
// the configured capability set. A non-Linux daemon cannot apply Linux
// capabilities at all, so that is refused here; every other capability
// decision stays Docker's, and a create-time rejection is reported as
// container_start_failed in phase start.
func (b *dockerBackend) CheckCapabilities(ctx context.Context, plan Plan) error {
	wanted := 0
	for _, unit := range plan.Units {
		wanted += len(unit.CapAdd)
	}
	if wanted == 0 {
		return nil
	}
	version, err := b.api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return wrapf(err, CodeDockerUnavailable, PhaseValidate, "server version: %v", err)
	}
	if version.Os != "linux" {
		return failf(CodeCapabilityUnsupported, PhaseValidate,
			"the daemon runs on %q and cannot apply Linux capabilities", version.Os)
	}
	return nil
}

func (b *dockerBackend) Prepare(ctx context.Context, plan Plan) error {
	return PullAndInspect(ctx, b.api, plan)
}

// Launch creates and starts one unit's container through stock
// testcontainers-go. Started is false and the request carries no wait
// strategy, so the readiness wait happens in WaitHealthy instead -- that is
// what lets a creation failure and a health failure carry different public
// codes and phases.
//
// Both error paths hand back whatever container exists. The library returns a
// live handle alongside its error when creation got as far as a container and
// a later step failed, and dropping it would strand a labelled container that
// cleanup then reports as successfully removed.
func (b *dockerBackend) Launch(ctx context.Context, plan Plan, unit Unit) (Instance, error) {
	created, err := newContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: buildContainerRequest(plan, unit),
		Started:          false,
	})
	var inst Instance
	if created != nil {
		inst = &dockerInstance{container: created}
	}
	if err != nil {
		// A nil handle does not mean nothing was created. The library creates
		// the container in the engine and only builds the handle once every
		// network is attached, so a failed attachment leaves a labelled
		// container with no handle to terminate. Reclaim it by label instead
		// of leaving it for the session reaper.
		if inst == nil {
			b.removeUnitContainers(ctx, plan.RunID, unit)
		}
		return inst, wrapf(err, CodeContainerStartFailed, PhaseStart,
			"create container for unit %s: %v", unit.DeviceIndices(), err)
	}
	if err := created.Start(ctx); err != nil {
		return inst, wrapf(err, CodeContainerStartFailed, PhaseStart,
			"start container for unit %s: %v", unit.DeviceIndices(), err)
	}
	return inst, nil
}

// WaitHealthy blocks on the image's declared HEALTHCHECK using the stock
// strategy. Container logs are never consulted for readiness.
func (b *dockerBackend) WaitHealthy(ctx context.Context, inst Instance) error {
	target, ok := inst.(*dockerInstance)
	if !ok {
		return fmt.Errorf("herder: instance %s is not a Docker container", inst.ID())
	}
	if err := healthWait().WaitUntilReady(ctx, target.container); err != nil {
		return wrapf(err, CodeDeviceUnhealthy, PhaseHealth, "wait for health: %v", err)
	}
	return nil
}

// Inspect reads the container state the herder acts on. It reduces the Docker
// response rather than passing it around, so the state machine stays testable
// without a daemon.
func (b *dockerBackend) Inspect(ctx context.Context, id string) (ContainerState, error) {
	got, err := b.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return ContainerState{}, wrapf(err, CodeDockerUnavailable, PhaseRuntime,
			"inspect container: %v", err)
	}
	state := ContainerState{RestartCount: got.Container.RestartCount}
	if got.Container.State != nil {
		state.Running = got.Container.State.Running
		state.ExitCode = got.Container.State.ExitCode
		state.StartedAt = got.Container.State.StartedAt
		if got.Container.State.Health != nil {
			state.Health = string(got.Container.State.Health.Status)
		}
	}
	if settings := got.Container.NetworkSettings; settings != nil {
		state.Networks = make(map[string]Endpoint, len(settings.Networks))
		for name, endpoint := range settings.Networks {
			if endpoint == nil {
				continue
			}
			ipv4 := ""
			if endpoint.IPAddress.Is4() {
				ipv4 = endpoint.IPAddress.String()
			}
			state.Networks[name] = Endpoint{ipv4, endpoint.MacAddress.String()}
		}
		for _, bindings := range settings.Ports {
			state.HostPortBindings += len(bindings)
		}
	}
	return state, nil
}

// Remaining counts containers still carrying this run's label. Labels do not
// perform cleanup; this is how the herder proves cleanup actually happened.
func (b *dockerBackend) Remaining(ctx context.Context, runID string) (int, error) {
	listed, err := b.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelRun+"="+runID),
	})
	if err != nil {
		return 0, wrapf(err, CodeDockerUnavailable, PhaseCleanup, "list run containers: %v", err)
	}
	return len(listed.Items), nil
}

// reclaimTimeout bounds the best-effort sweep of a container that was created
// before the library could hand back a handle.
const reclaimTimeout = 30 * time.Second

// removeUnitContainers force-removes anything already carrying this run and
// unit's labels. Best effort: the caller is already reporting a failure, and
// cleanup re-checks what remains afterwards.
func (b *dockerBackend) removeUnitContainers(parent context.Context, runID string, unit Unit) {
	// The commonest reason creation failed is that this context died -- the
	// startup deadline expired, or a signal arrived. Reusing it would make the
	// reclaim a no-op in exactly the cases that strand a container.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), reclaimTimeout)
	defer cancel()
	listed, err := b.api.ContainerList(ctx, client.ContainerListOptions{
		All: true,
		// Narrow on purpose. The handle is built before anything is started,
		// so an orphan from this path is always still in "created" -- and a
		// running container carrying these labels belongs to someone else.
		Filters: make(client.Filters).
			Add("label", labelRun+"="+runID).
			Add("label", labelDevices+"="+unit.DeviceIndices()).
			Add("status", "created"),
	})
	if err != nil {
		return
	}
	for _, c := range listed.Items {
		_, _ = b.api.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
	}
}

// dockerInstance is one testcontainers-managed device container.
type dockerInstance struct {
	container testcontainers.Container
}

func (d *dockerInstance) ID() string { return d.container.GetContainerID() }

// Terminate performs the whole documented per-container sequence: stop, wait
// up to the stop timeout, then force-remove. The caller's controller and
// network are never registered with this session, so they are never touched.
func (d *dockerInstance) Terminate(ctx context.Context, stopTimeout time.Duration) error {
	return d.container.Terminate(ctx, testcontainers.StopTimeout(stopTimeout))
}

func (d *dockerInstance) Logs(ctx context.Context) (io.ReadCloser, error) {
	return d.container.Logs(ctx)
}
