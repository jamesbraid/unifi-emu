// Package dockerhost finds the Docker daemon the docker CLI would talk to.
//
// Testcontainers looks at DOCKER_HOST, its own properties file and a few
// well-known socket paths, but not at docker contexts. Colima and Docker
// Desktop install a context and no /var/run/docker.sock, so on those hosts
// `docker` works while anything built on Testcontainers reports that Docker
// is missing. Resolve closes that gap by reading the context the same way the
// CLI does.
package dockerhost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Endpoint is where Resolve found Docker.
type Endpoint struct {
	// Host is the daemon address, e.g. unix:///Users/me/.colima/default/docker.sock.
	// Empty means nothing names a daemon and the caller's own discovery applies.
	Host string
	// Context is the docker context that named Host. It is empty when Host
	// came from DOCKER_HOST, or when no context applies.
	Context string
	// Source says what chose Host, for error messages.
	Source string
}

// Resolve applies the docker CLI's order: DOCKER_HOST, then DOCKER_CONTEXT,
// then currentContext in the docker config file. The "default" context, or
// no context at all, yields an empty Endpoint: the CLI's default is the
// platform socket, which Testcontainers already probes.
//
// getenv is os.Getenv in production. home locates ~/.docker when
// DOCKER_CONFIG is unset; empty skips the config file.
func Resolve(getenv func(string) string, home string) (Endpoint, error) {
	if host := getenv("DOCKER_HOST"); host != "" {
		return Endpoint{Host: host, Source: "DOCKER_HOST"}, nil
	}

	configDir := getenv("DOCKER_CONFIG")
	if configDir == "" && home != "" {
		configDir = filepath.Join(home, ".docker")
	}

	name, from := getenv("DOCKER_CONTEXT"), "DOCKER_CONTEXT"
	if name == "" && configDir != "" {
		configFile := filepath.Join(configDir, "config.json")
		current, err := currentContext(configFile)
		if err != nil {
			return Endpoint{}, err
		}
		name, from = current, "currentContext in "+configFile
	}
	if name == "" || name == "default" {
		return Endpoint{}, nil
	}

	source := fmt.Sprintf("docker context %q (%s)", name, from)
	if configDir == "" {
		return Endpoint{}, fmt.Errorf("%s: no docker config directory: set DOCKER_CONFIG or HOME", source)
	}
	host, err := contextHost(configDir, name)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%s: %w", source, err)
	}
	return Endpoint{Host: host, Context: name, Source: source}, nil
}

// currentContext reads currentContext from a docker config file. A missing
// file is not an error: it is the state of a host where docker was never
// configured.
func currentContext(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read docker config: %w", err)
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse docker config %s: %w", path, err)
	}
	return cfg.CurrentContext, nil
}

// contextHost reads a context's Docker endpoint from the CLI's metadata
// store, where each context lives in a directory named for the SHA-256 of
// its name.
func contextHost(configDir, name string) (string, error) {
	sum := sha256.Sum256([]byte(name))
	path := filepath.Join(configDir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("context not found (no %s)", path)
	}
	if err != nil {
		return "", fmt.Errorf("read context metadata: %w", err)
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parse context metadata %s: %w", path, err)
	}
	host := meta.Endpoints["docker"].Host
	if host == "" {
		return "", fmt.Errorf("context has no docker endpoint (%s)", path)
	}
	return host, nil
}

// vmSocketPaths mark sockets that a VM forwards to the host. Containers run
// inside that VM, where the daemon's socket is /var/run/docker.sock and the
// host path does not exist, so anything that bind-mounts the socket (the
// Testcontainers reaper) must be told the in-VM path instead.
var vmSocketPaths = []string{
	"/.colima/",           // colima
	"/.docker/run/",       // Docker Desktop on macOS
	"/.docker/desktop/",   // Docker Desktop on Linux
	"/com.docker.docker/", // older Docker Desktop on macOS
}

// SocketOverride returns the path containers should mount for host, or ""
// when the host path is the real one.
func SocketOverride(host string) string {
	if !strings.HasPrefix(host, "unix://") {
		return ""
	}
	for _, marker := range vmSocketPaths {
		if strings.Contains(host, marker) {
			return "/var/run/docker.sock"
		}
	}
	return ""
}
