package herder

import (
	"context"
	"errors"
	"testing"

	"github.com/testcontainers/testcontainers-go"
)

// stubContainer is a created container. Only the calls Launch makes are
// implemented; the embedded nil interface makes any other call panic loudly
// rather than quietly returning a zero value.
type stubContainer struct{ testcontainers.Container }

func (stubContainer) GetContainerID() string { return "created-anyway" }

// The container library returns a live handle alongside its error when a
// container was created and a later step failed -- a reaper connection, a
// post-create hook. Dropping that handle strands a labelled container that
// cleanup then reports as complete, so Launch must hand it back with the
// error and let the run register it for teardown.
func TestLaunchReturnsTheHandleWhenCreationFailsAfterTheContainerExists(t *testing.T) {
	previous := newContainer
	defer func() { newContainer = previous }()
	newContainer = func(context.Context, testcontainers.GenericContainerRequest) (testcontainers.Container, error) {
		return stubContainer{}, errors.New("reaper connection failed")
	}

	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	backend := &dockerBackend{api: &fakeDocker{}}
	inst, err := backend.Launch(context.Background(), plan, plan.Units[0])
	if err == nil {
		t.Fatal("Launch reported success on a failed creation")
	}
	if inst == nil {
		t.Fatal("Launch discarded the created container, so cleanup cannot remove it")
	}
	if inst.ID() != "created-anyway" {
		t.Fatalf("instance id = %q, want the created container", inst.ID())
	}
	assertFailure(t, err, CodeContainerStartFailed, PhaseStart)
}

// A creation that produced nothing still hands back no instance -- but see
// TestLaunchReclaimsAContainerCreatedWithoutAHandle: a nil handle does not by
// itself prove nothing was created.
func TestLaunchReturnsNoInstanceWhenNothingWasCreated(t *testing.T) {
	previous := newContainer
	defer func() { newContainer = previous }()
	newContainer = func(context.Context, testcontainers.GenericContainerRequest) (testcontainers.Container, error) {
		return nil, errors.New("no such image")
	}

	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	api := &fakeDocker{}
	backend := &dockerBackend{api: api}
	inst, err := backend.Launch(context.Background(), plan, plan.Units[0])
	if err == nil {
		t.Fatal("Launch reported success on a failed creation")
	}
	if inst != nil {
		t.Fatalf("instance = %#v, want none", inst)
	}
	if len(api.removed) != 0 {
		t.Fatalf("removed = %#v, want nothing: no container carried the labels", api.removed)
	}
}

// The library creates the container in the engine and only builds the handle
// once every network is attached, so a failed attachment returns (nil, error)
// with a labelled container already running. Launch must reclaim it by label;
// leaving it strands a container cleanup can detect but not remove.
func TestLaunchReclaimsAContainerCreatedWithoutAHandle(t *testing.T) {
	previous := newContainer
	defer func() { newContainer = previous }()
	newContainer = func(context.Context, testcontainers.GenericContainerRequest) (testcontainers.Container, error) {
		return nil, errors.New("network connect: no such network")
	}

	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	mine := map[string]string{labelHerder: "true", labelRun: "run1", labelDevices: "0"}
	api := &fakeDocker{containers: []fakeContainer{
		{id: "orphan-1", labels: mine, status: "created"},
		// Decoys. A sweep that filtered too loosely would take these with it:
		// another run's container, another unit's, and this run's own healthy
		// one -- which cannot be an orphan, because the handle is built before
		// anything is started.
		{id: "other-run", labels: map[string]string{
			labelHerder: "true", labelRun: "run2", labelDevices: "0"}, status: "created"},
		{id: "other-unit", labels: map[string]string{
			labelHerder: "true", labelRun: "run1", labelDevices: "1"}, status: "created"},
		{id: "running-sibling", labels: mine, status: "running"},
		{id: "unrelated", labels: map[string]string{}, status: "created"},
	}}
	backend := &dockerBackend{api: api}
	inst, err := backend.Launch(context.Background(), plan, plan.Units[0])
	if err == nil {
		t.Fatal("Launch reported success")
	}
	if inst != nil {
		t.Fatalf("instance = %#v, want none: the library built no handle", inst)
	}
	if len(api.removed) != 1 || api.removed[0] != "orphan-1" {
		t.Fatalf("removed = %#v, want only this launch's orphan", api.removed)
	}
}

// The reclaim runs on the way out of a failure, and the commonest reason
// creation failed is that the context died -- the startup deadline expired or
// a signal arrived. Reusing that context would make every reclaim a no-op in
// exactly the cases that produce an orphan.
func TestReclaimRunsEvenWhenTheContextIsAlreadyDone(t *testing.T) {
	previous := newContainer
	defer func() { newContainer = previous }()
	newContainer = func(context.Context, testcontainers.GenericContainerRequest) (testcontainers.Container, error) {
		return nil, context.DeadlineExceeded
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the deadline that killed creation is already gone

	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	api := &fakeDocker{requireLiveContext: true, containers: []fakeContainer{
		{id: "orphan-2", status: "created", labels: map[string]string{
			labelHerder: "true", labelRun: "run1", labelDevices: "0"}},
	}}
	backend := &dockerBackend{api: api}
	if _, err := backend.Launch(ctx, plan, plan.Units[0]); err == nil {
		t.Fatal("Launch reported success")
	}
	if len(api.removed) != 1 || api.removed[0] != "orphan-2" {
		t.Fatalf("removed = %#v, want the orphan reclaimed despite the dead context", api.removed)
	}
}
