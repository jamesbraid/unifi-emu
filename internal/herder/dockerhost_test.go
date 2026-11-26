package herder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProcess is an environment and a docker config directory that
// selectDocker reads and writes instead of the real ones.
type fakeProcess struct {
	vars    map[string]string
	pinged  []string
	pingErr error
}

func (p *fakeProcess) env(home string) dockerEnv {
	return dockerEnv{
		getenv: func(k string) string { return p.vars[k] },
		setenv: func(k, v string) error { p.vars[k] = v; return nil },
		home:   home,
		ping: func(_ context.Context, host string) error {
			p.pinged = append(p.pinged, host)
			return p.pingErr
		},
	}
}

// writeDockerContext lays out a current context the way the docker CLI does.
func writeDockerContext(t *testing.T, configDir, name, host string) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	meta := filepath.Join(configDir, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(configDir, "config.json"): `{"currentContext":"` + name + `"}`,
		filepath.Join(meta, "meta.json"):        `{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `"}}}`,
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

const testColimaHost = "unix:///Users/test/.colima/default/docker.sock"

func TestSelectDockerExportsTheCurrentContext(t *testing.T) {
	home := t.TempDir()
	writeDockerContext(t, filepath.Join(home, ".docker"), "colima", testColimaHost)
	p := &fakeProcess{vars: map[string]string{}}

	ep, err := selectDocker(context.Background(), p.env(home))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Context != "colima" {
		t.Fatalf("context = %q, want colima", ep.Context)
	}
	if p.vars["DOCKER_HOST"] != testColimaHost {
		t.Errorf("DOCKER_HOST = %q, want %q", p.vars["DOCKER_HOST"], testColimaHost)
	}
	if p.vars["TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"] != "/var/run/docker.sock" {
		t.Errorf("socket override = %q, want /var/run/docker.sock",
			p.vars["TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"])
	}
	if len(p.pinged) != 1 || p.pinged[0] != testColimaHost {
		t.Errorf("pinged %v, want exactly the context endpoint", p.pinged)
	}
}

func TestSelectDockerNamesAnUnreachableContext(t *testing.T) {
	home := t.TempDir()
	writeDockerContext(t, filepath.Join(home, ".docker"), "colima", testColimaHost)
	p := &fakeProcess{vars: map[string]string{}, pingErr: errors.New("connection refused")}

	_, err := selectDocker(context.Background(), p.env(home))
	var he *Failure
	if !errors.As(err, &he) || he.Code != CodeDockerUnavailable || he.Phase != PhaseValidate {
		t.Fatalf("error = %v, want docker_unavailable in validate", err)
	}
	for _, want := range []string{`docker context "colima"`, testColimaHost, "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// Exporting it would let Testcontainers fall through to another daemon.
	if _, set := p.vars["DOCKER_HOST"]; set {
		t.Errorf("DOCKER_HOST was set to %q for an unreachable context", p.vars["DOCKER_HOST"])
	}
}

func TestSelectDockerLeavesAnExplicitDockerHostAlone(t *testing.T) {
	home := t.TempDir()
	writeDockerContext(t, filepath.Join(home, ".docker"), "colima", testColimaHost)
	p := &fakeProcess{vars: map[string]string{"DOCKER_HOST": "tcp://10.0.0.5:2375"}}

	if _, err := selectDocker(context.Background(), p.env(home)); err != nil {
		t.Fatal(err)
	}
	if p.vars["DOCKER_HOST"] != "tcp://10.0.0.5:2375" {
		t.Errorf("DOCKER_HOST = %q, want the caller's value", p.vars["DOCKER_HOST"])
	}
	if len(p.pinged) != 0 {
		t.Errorf("pinged %v; an explicit DOCKER_HOST is Testcontainers' to probe", p.pinged)
	}
	if _, set := p.vars["TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"]; set {
		t.Error("set a socket override for a TCP host")
	}
}

func TestSelectDockerOverridesTheSocketForAnExplicitColimaHost(t *testing.T) {
	p := &fakeProcess{vars: map[string]string{"DOCKER_HOST": testColimaHost}}
	if _, err := selectDocker(context.Background(), p.env("")); err != nil {
		t.Fatal(err)
	}
	if p.vars["TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"] != "/var/run/docker.sock" {
		t.Errorf("socket override = %q, want /var/run/docker.sock",
			p.vars["TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"])
	}
}

func TestSelectDockerKeepsAnExplicitSocketOverride(t *testing.T) {
	p := &fakeProcess{vars: map[string]string{
		"DOCKER_HOST":                           testColimaHost,
		"TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE": "/custom.sock",
	}}
	if _, err := selectDocker(context.Background(), p.env("")); err != nil {
		t.Fatal(err)
	}
	if p.vars["TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"] != "/custom.sock" {
		t.Errorf("socket override = %q, want the caller's /custom.sock",
			p.vars["TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"])
	}
}

func TestSelectDockerWithNoContextChangesNothing(t *testing.T) {
	p := &fakeProcess{vars: map[string]string{}}
	ep, err := selectDocker(context.Background(), p.env(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Host != "" || len(p.vars) != 0 || len(p.pinged) != 0 {
		t.Fatalf("endpoint %+v, env %v, pinged %v; want today's discovery untouched", ep, p.vars, p.pinged)
	}
}

func TestSelectDockerReportsABrokenContextAsDockerUnavailable(t *testing.T) {
	p := &fakeProcess{vars: map[string]string{"DOCKER_CONTEXT": "ghost", "DOCKER_CONFIG": t.TempDir()}}
	_, err := selectDocker(context.Background(), p.env(""))
	var he *Failure
	if !errors.As(err, &he) || he.Code != CodeDockerUnavailable {
		t.Fatalf("error = %v, want docker_unavailable", err)
	}
	if !strings.Contains(err.Error(), `docker context "ghost"`) {
		t.Errorf("error %q does not name the context", err)
	}
}
