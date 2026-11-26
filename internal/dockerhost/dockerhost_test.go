package dockerhost

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDocker is a docker config directory laid out the way the CLI writes it.
type fakeDocker struct {
	t   *testing.T
	dir string
}

func newFakeDocker(t *testing.T) fakeDocker {
	t.Helper()
	return fakeDocker{t: t, dir: t.TempDir()}
}

func (f fakeDocker) write(rel, body string) {
	f.t.Helper()
	path := filepath.Join(f.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f fakeDocker) current(name string) {
	f.write("config.json", `{"auths":{},"currentContext":"`+name+`"}`)
}

func (f fakeDocker) context(name, host string) {
	sum := sha256.Sum256([]byte(name))
	f.write(filepath.Join("contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"),
		`{"Name":"`+name+`","Metadata":{},"Endpoints":{"docker":{"Host":"`+host+`","SkipTLSVerify":false}}}`)
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

const (
	colimaHost  = "unix:///Users/test/.colima/default/docker.sock"
	desktopHost = "unix:///Users/test/.docker/run/docker.sock"
)

func TestDockerHostWinsOverEveryContext(t *testing.T) {
	f := newFakeDocker(t)
	f.current("colima")
	f.context("colima", colimaHost)
	got, err := Resolve(env(map[string]string{
		"DOCKER_HOST":    "tcp://10.0.0.5:2375",
		"DOCKER_CONTEXT": "colima",
		"DOCKER_CONFIG":  f.dir,
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	want := Endpoint{Host: "tcp://10.0.0.5:2375", Source: "DOCKER_HOST"}
	if got != want {
		t.Fatalf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestDockerContextWinsOverTheCurrentContext(t *testing.T) {
	f := newFakeDocker(t)
	f.current("colima")
	f.context("colima", colimaHost)
	f.context("desktop-linux", desktopHost)
	got, err := Resolve(env(map[string]string{
		"DOCKER_CONTEXT": "desktop-linux",
		"DOCKER_CONFIG":  f.dir,
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != desktopHost || got.Context != "desktop-linux" {
		t.Fatalf("Resolve() = %+v, want desktop-linux at %s", got, desktopHost)
	}
	if !strings.Contains(got.Source, "DOCKER_CONTEXT") {
		t.Fatalf("Source = %q, want it to name DOCKER_CONTEXT", got.Source)
	}
}

func TestCurrentContextIsReadFromDockerConfig(t *testing.T) {
	f := newFakeDocker(t)
	f.current("colima")
	f.context("colima", colimaHost)
	got, err := Resolve(env(map[string]string{"DOCKER_CONFIG": f.dir}), "/nonexistent-home")
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != colimaHost || got.Context != "colima" {
		t.Fatalf("Resolve() = %+v, want colima at %s", got, colimaHost)
	}
	if !strings.Contains(got.Source, filepath.Join(f.dir, "config.json")) {
		t.Fatalf("Source = %q, want it to name the config file", got.Source)
	}
}

func TestCurrentContextDefaultsToHomeDotDocker(t *testing.T) {
	home := t.TempDir()
	f := fakeDocker{t: t, dir: filepath.Join(home, ".docker")}
	f.current("colima")
	f.context("colima", colimaHost)
	got, err := Resolve(env(nil), home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != colimaHost {
		t.Fatalf("Resolve() = %+v, want %s", got, colimaHost)
	}
}

func TestNoContextLeavesDiscoveryToTheCaller(t *testing.T) {
	cases := map[string]func(fakeDocker){
		"no config file":         func(fakeDocker) {},
		"no currentContext":      func(f fakeDocker) { f.write("config.json", `{"auths":{}}`) },
		"the default context":    func(f fakeDocker) { f.current("default") },
		"an empty config object": func(f fakeDocker) { f.write("config.json", `{}`) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeDocker(t)
			setup(f)
			got, err := Resolve(env(map[string]string{"DOCKER_CONFIG": f.dir}), "")
			if err != nil {
				t.Fatal(err)
			}
			if got != (Endpoint{}) {
				t.Fatalf("Resolve() = %+v, want an empty Endpoint", got)
			}
		})
	}
}

func TestNoHomeAndNoDockerConfigSkipsTheConfigFile(t *testing.T) {
	got, err := Resolve(env(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != (Endpoint{}) {
		t.Fatalf("Resolve() = %+v, want an empty Endpoint", got)
	}
}

func TestBrokenContextsNameTheContextAndTheFile(t *testing.T) {
	cases := []struct {
		name  string
		vars  map[string]string
		setup func(fakeDocker)
		want  []string
	}{
		{
			name:  "a current context with no metadata",
			setup: func(f fakeDocker) { f.current("colima") },
			want:  []string{`docker context "colima"`, "currentContext in", "context not found", "meta.json"},
		},
		{
			name:  "a DOCKER_CONTEXT with no metadata",
			vars:  map[string]string{"DOCKER_CONTEXT": "ghost"},
			setup: func(fakeDocker) {},
			want:  []string{`docker context "ghost" (DOCKER_CONTEXT)`, "context not found"},
		},
		{
			name: "metadata without a docker endpoint",
			setup: func(f fakeDocker) {
				f.current("k8s")
				sum := sha256.Sum256([]byte("k8s"))
				f.write(filepath.Join("contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"),
					`{"Name":"k8s","Endpoints":{"kubernetes":{"Host":"https://k8s"}}}`)
			},
			want: []string{`docker context "k8s"`, "no docker endpoint"},
		},
		{
			name:  "a malformed config file",
			setup: func(f fakeDocker) { f.write("config.json", `{"currentContext":`) },
			want:  []string{"parse docker config", "config.json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker(t)
			tc.setup(f)
			vars := map[string]string{"DOCKER_CONFIG": f.dir}
			maps.Copy(vars, tc.vars)
			_, err := Resolve(env(vars), "")
			if err == nil {
				t.Fatal("Resolve() succeeded, want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestDockerContextWithNoConfigDirectoryIsAnError(t *testing.T) {
	_, err := Resolve(env(map[string]string{"DOCKER_CONTEXT": "colima"}), "")
	if err == nil || !strings.Contains(err.Error(), `docker context "colima"`) {
		t.Fatalf("Resolve() error = %v, want one naming the context", err)
	}
}

func TestSocketOverrideForVMBackedSockets(t *testing.T) {
	for _, host := range []string{
		colimaHost,
		"unix:///Users/test/.colima/work/docker.sock",
		desktopHost,
		"unix:///home/test/.docker/desktop/docker.sock",
		"unix:///Users/test/Library/Containers/com.docker.docker/Data/docker.raw.sock",
	} {
		if got := SocketOverride(host); got != "/var/run/docker.sock" {
			t.Errorf("SocketOverride(%q) = %q, want /var/run/docker.sock", host, got)
		}
	}
}

func TestSocketOverrideLeavesOtherHostsAlone(t *testing.T) {
	for _, host := range []string{
		"",
		"unix:///var/run/docker.sock",
		"unix:///run/user/1000/docker.sock",
		"tcp://10.0.0.5:2375",
		"ssh://me@builder",
	} {
		if got := SocketOverride(host); got != "" {
			t.Errorf("SocketOverride(%q) = %q, want no override", host, got)
		}
	}
}
