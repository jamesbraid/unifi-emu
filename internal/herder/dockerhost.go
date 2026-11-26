package herder

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/moby/moby/client"

	"github.com/jamesbraid/unifi-emu/internal/dockerhost"
)

// dockerEnv is the process environment selectDocker reads and writes,
// indirected so the selection order can be tested without touching the
// real environment.
type dockerEnv struct {
	getenv func(string) string
	setenv func(string, string) error
	home   string
	ping   func(ctx context.Context, host string) error
}

func processDockerEnv() dockerEnv {
	home, _ := os.UserHomeDir() // no home only skips ~/.docker
	return dockerEnv{getenv: os.Getenv, setenv: os.Setenv, home: home, ping: pingDocker}
}

// selectDocker points Testcontainers at the daemon the docker CLI would use.
// Testcontainers reads only the environment, and caches what it finds on
// first use, so this must run before any Testcontainers call.
//
// A context's endpoint is checked here rather than handed over blind:
// Testcontainers treats an unreachable DOCKER_HOST as one candidate among
// several and moves on to the next, so a dead context would quietly become
// some other daemon instead of an error that names it.
func selectDocker(ctx context.Context, e dockerEnv) (dockerhost.Endpoint, error) {
	ep, err := dockerhost.Resolve(e.getenv, e.home)
	if err != nil {
		return ep, wrapf(err, CodeDockerUnavailable, PhaseValidate, "find Docker: %v", err)
	}
	if ep.Context != "" {
		if err := e.ping(ctx, ep.Host); err != nil {
			return ep, wrapf(err, CodeDockerUnavailable, PhaseValidate,
				"connect to Docker: %s endpoint %s: %v", ep.Source, ep.Host, err)
		}
		if err := e.setenv("DOCKER_HOST", ep.Host); err != nil {
			return ep, wrapf(err, CodeDockerUnavailable, PhaseValidate, "select %s: %v", ep.Source, err)
		}
	}
	// The reaper bind-mounts the daemon socket into a container, which sees
	// the VM's filesystem, not the host's. An explicit override still wins.
	if override := dockerhost.SocketOverride(ep.Host); override != "" &&
		e.getenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE") == "" {
		if err := e.setenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", override); err != nil {
			return ep, wrapf(err, CodeDockerUnavailable, PhaseValidate, "set the socket override: %v", err)
		}
	}
	return ep, nil
}

// pingDocker reports whether a daemon answers at host.
func pingDocker(ctx context.Context, host string) error {
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = cli.Ping(ctx, client.PingOptions{})
	return err
}

// describeEndpoint names where a connection attempt went, for error messages.
func describeEndpoint(ep dockerhost.Endpoint) string {
	if ep.Host == "" {
		return ""
	}
	return fmt.Sprintf(" at %s (%s)", ep.Host, ep.Source)
}
