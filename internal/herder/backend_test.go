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
	backend := &dockerBackend{}
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

// A creation that produced nothing has nothing to hand back.
func TestLaunchReturnsNoInstanceWhenNothingWasCreated(t *testing.T) {
	previous := newContainer
	defer func() { newContainer = previous }()
	newContainer = func(context.Context, testcontainers.GenericContainerRequest) (testcontainers.Container, error) {
		return nil, errors.New("no such image")
	}

	plan := buildPlan(t, ids("USM8P"), RuntimeConfig{}, "public/emu:1.0.0")
	backend := &dockerBackend{}
	inst, err := backend.Launch(context.Background(), plan, plan.Units[0])
	if err == nil {
		t.Fatal("Launch reported success on a failed creation")
	}
	if inst != nil {
		t.Fatalf("instance = %#v, want none", inst)
	}
}
