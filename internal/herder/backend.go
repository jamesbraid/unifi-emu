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
// testcontainers-go. Started is false so the library's readiness wait happens
// in WaitHealthy instead, which is what lets a creation failure and a health
// failure carry different public codes and phases.
func (b *dockerBackend) Launch(ctx context.Context, plan Plan, unit Unit) (Instance, error) {
	created, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: buildContainerRequest(plan, unit),
		Started:          false,
	})
	if err != nil {
		return nil, wrapf(err, CodeContainerStartFailed, PhaseStart,
			"create container for unit %s: %v", unit.DeviceIndices(), err)
	}
	inst := &dockerInstance{container: created}
	if err := created.Start(ctx); err != nil {
		// Hand the caller the instance anyway: it exists, so cleanup owns it.
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
	if err := wait.ForHealthCheck().WaitUntilReady(ctx, target.container); err != nil {
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
