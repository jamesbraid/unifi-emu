// Command runtime-fixture is a public stand-in for the opaque OCI runtime
// images the herder starts for some device models. It implements the whole
// container contract and nothing else, so the herder's tests can prove that
// contract end to end without any image the repository cannot ship.
//
// It is deliberately dumb: two environment variables in, a periodic JSON
// POST out, a readiness marker for HEALTHCHECK, and a clean exit on
// SIGTERM. Any input it cannot honour is fatal — a runtime that invents an
// identity is worse than one that never starts, because the invented one
// still reaches the controller.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The two variables the herder sets, and the only two it may set.
const (
	envInformURL  = "UNIFI_EMU_INFORM_URL"
	envDevicesRaw = "UNIFI_EMU_DEVICES_JSON"
	envPrefix     = "UNIFI_EMU_"
)

// markerPath sits at the filesystem root because a scratch image has no
// /tmp, /var or /run to write into.
const markerPath = "/ready"

const informEvery = 2 * time.Second

// device is the identity the herder assigns. The json tags are the wire
// contract for UNIFI_EMU_DEVICES_JSON; decoding is strict, so this list is
// exhaustive and a new field is a breaking change on both ends at once.
type device struct {
	Model  string `json:"model"`
	MAC    string `json:"mac"`
	Serial string `json:"serial"`
	Name   string `json:"name"`
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the readiness marker and exit 0 (healthy) or 1")
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("runtime-fixture: ")

	if *healthcheck {
		if err := probe(markerPath, processAlive); err != nil {
			log.Printf("unhealthy: %v", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	warnExtraEnv(os.Environ(), log.Printf)

	informTo, err := informURL(os.Getenv(envInformURL))
	if err != nil {
		return err
	}
	dev, err := decodeDevice(os.Getenv(envDevicesRaw))
	if err != nil {
		return err
	}
	ip, err := containerIPv4(net.InterfaceAddrs)
	if err != nil {
		return err
	}
	body, err := informBody(dev, ip, informTo)
	if err != nil {
		return err
	}

	// Only now is the input known good, so this is the earliest point the
	// container may report healthy.
	if err := writeMarker(markerPath, os.Getpid()); err != nil {
		return err
	}
	defer os.Remove(markerPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log.Printf("informing %s as %s (%s, %s) from %s", informTo, dev.Name, dev.Model, dev.MAC, ip)
	informLoop(ctx, informEvery, poster(informTo, body), log.Printf)
	return nil
}

// decodeDevice parses UNIFI_EMU_DEVICES_JSON, which is always an array but
// must carry exactly one device: the herder runs one container per device,
// so zero or two means the two ends disagree about what this container is.
// Decoding is strict — an unknown key is a herder that grew a field this
// runtime silently ignores, which is exactly the drift worth failing on.
func decodeDevice(raw string) (device, error) {
	if strings.TrimSpace(raw) == "" {
		return device{}, fmt.Errorf("%s is not set", envDevicesRaw)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var devices []device
	if err := dec.Decode(&devices); err != nil {
		return device{}, fmt.Errorf("parse %s: %w", envDevicesRaw, err)
	}
	if dec.More() {
		return device{}, fmt.Errorf("parse %s: trailing data after the device array", envDevicesRaw)
	}
	if len(devices) != 1 {
		return device{}, fmt.Errorf("%s: want exactly one device, got %d", envDevicesRaw, len(devices))
	}
	return devices[0], nil
}

// informURL validates the herder's inform target. There is no default to
// fall back on, so anything unusable is fatal before the marker is written.
func informURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%s is not set", envInformURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", envInformURL, raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%s %q: want http(s)://host:port/path", envInformURL, raw)
	}
	return raw, nil
}

// warnExtraEnv flags any other UNIFI_EMU_* variable. The contract is two
// variables and nothing else; a third one means the herder is passing
// configuration a real opaque runtime would ignore, which is worth seeing
// in the logs. It is not fatal — the two that matter are still honoured.
func warnExtraEnv(environ []string, logf func(string, ...any)) {
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, envPrefix) && k != envInformURL && k != envDevicesRaw {
			logf("ignoring unexpected %s (contract is %s and %s only)", k, envInformURL, envDevicesRaw)
		}
	}
}

// containerIPv4 returns the Docker-assigned endpoint address of this
// container: the first non-loopback IPv4 the interfaces carry. 169.254/16
// is skipped because that is what a container ends up with when no network
// is attached — reporting it would send the controller somewhere useless.
func containerIPv4(addrs func() ([]net.Addr, error)) (string, error) {
	all, err := addrs()
	if err != nil {
		return "", fmt.Errorf("enumerate interfaces: %w", err)
	}
	for _, a := range all {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		v4 := ip.To4()
		if v4 == nil || v4.IsLoopback() || v4.IsLinkLocalUnicast() {
			continue
		}
		return v4.String(), nil
	}
	return "", errors.New("no non-loopback IPv4 address on any interface")
}

// informBody is what the runtime reports: the herder's identities plus the
// one fact only the container knows, its endpoint IP.
func informBody(d device, ip, informTo string) ([]byte, error) {
	return json.Marshal(map[string]string{
		"model":      d.Model,
		"mac":        d.MAC,
		"serial":     d.Serial,
		"name":       d.Name,
		"ip":         ip,
		"inform_url": informTo,
	})
}

// informLoop posts immediately and then every interval until ctx is done,
// so a container is useful the moment it reports ready. A failed post is
// logged and nothing more: health means this process is alive, not that the
// controller is up, and a controller that starts late must not take the
// fleet down with it.
func informLoop(ctx context.Context, every time.Duration, post func(context.Context) error, logf func(string, ...any)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := post(ctx); err != nil {
			logf("inform: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// poster binds the static body to one inform target. The client timeout is
// under the inform interval so a black-holed controller cannot stack
// requests, and ctx cancellation aborts an in-flight one so SIGTERM is
// prompt rather than timeout-bound.
func poster(informTo string, body []byte) func(context.Context) error {
	client := &http.Client{Timeout: informEvery}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, informTo, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil // shutting down, not a controller failure
			}
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body) // drain so the connection is reused
		if resp.StatusCode >= 400 {
			return fmt.Errorf("POST %s: %s", informTo, resp.Status)
		}
		return nil
	}
}

// writeMarker records the informing process's pid, which is what lets the
// probe tell "ready" from a marker a dead process left behind.
func writeMarker(path string, pid int) error {
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("write readiness marker: %w", err)
	}
	return nil
}

// probe is the HEALTHCHECK: healthy only while the process that validated
// the input and started informing is still running. A scratch image has no
// shell to express this, so the binary probes itself.
func probe(path string, alive func(int) bool) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no readiness marker: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("readiness marker %q is not a pid", string(b))
	}
	if !alive(pid) {
		return fmt.Errorf("device process %d is gone", pid)
	}
	return nil
}

// processAlive uses signal 0, which the healthcheck can send because it
// runs in the container's own pid namespace as the same user.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
