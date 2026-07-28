// Command unifi-emu runs a fleet of emulated UniFi devices informing a
// real controller until interrupted. Device sources (mutually exclusive):
// -devices FILE (YAML/JSON), SIM_DEVICES env (inline YAML),
// UNIFI_EMU_DEVICES_JSON env (JSON array of model/mac/serial/name), -models
// (terse MODEL[:count] list), SIM_MODELS env, or the single-device flags.
// When none of those is set, SIM_DEFAULT_DEVICES (the image's baked-in
// fleet file) is used if present; any explicit source overrides it.
//
// UNIFI_EMU_DEVICES_JSON is the device-container contract: the caller has
// already allocated every identity, so the serial is reported as given and
// every device reports this container's own IPv4 address rather than an
// auto-allocated one. UNIFI_EMU_INFORM_URL names the controller for that
// contract (an explicit -inform still wins, SIM_CONTROLLER still works), and
// UNIFI_EMU_READY_FILE (default /unifi-emu-ready) is the readiness marker
// -healthcheck probes.
//
// With -adopt (or SIM_ADOPT=1) it also drives the fleet to adoption: after
// the devices start informing it logs into the controller's API and issues
// the same adopt command the UI does, so one container produces connected
// devices rather than pending ones. Adoption needs the controller's API port
// (8443 classic, 443 UniFi OS) on top of the inform port.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jamesbraid/unifi-emu"
)

// buildVersion is the CLI build version, stamped at release time via
// -ldflags "-X main.buildVersion=<ver>"; "dev" for local/unstamped builds.
var buildVersion = "dev"

func main() {
	os.Exit(run())
}

// run is main's body, returning the process exit code. main is a one-liner
// around it so a failed adoption can stop the fleet on the way out: a
// log.Fatal here would skip that and leave the controller holding devices
// that stopped informing without warning.
func run() int {
	// The adopt environment is read before the flags so it can supply their
	// defaults, but its error is held until after flag.Parse: -V and
	// -healthcheck must answer in a container whose environment is broken,
	// and a health probe that fails for its own reasons reports the prober.
	adoptEnv, adoptEnvErr := adoptEnvDefaults(os.Getenv)
	inform := flag.String("inform", informURLDefault(os.Getenv),
		"controller inform URL (default: env UNIFI_EMU_INFORM_URL, else SIM_CONTROLLER)")
	devices := flag.String("devices", "", "YAML/JSON file with an array of DeviceSpec (fleet mode; "+
		"keys: mac, serial, type, model, modeldisplay, version, name, ip, ports, ssids; unknown keys rejected). "+
		"Fleet sources (mutually exclusive): -devices FILE (YAML/JSON), SIM_DEVICES env (inline YAML list), "+
		"UNIFI_EMU_DEVICES_JSON env (JSON array of model/mac/serial/name), -models, or SIM_MODELS env; "+
		"any one beats single-device flags")
	models := flag.String("models", "", "terse fleet: comma-separated MODEL[:count] (e.g. U7PRO,USM8P:2,UGW3); "+
		"MAC/IP auto-derived from SIM_MAC_BASE/SIM_IP_BASE. Fleet sources are mutually exclusive.")
	mac := flag.String("mac", "00:27:22:e0:00:01", "device MAC (single-device mode)")
	typ := flag.String("type", "", "device type ugw/usw/uap (default: from model profile)")
	model := flag.String("model", "UGW3", "device model")
	modelDisplay := flag.String("model-display", "", "model display name (default: from model profile)")
	version := flag.String("version", "", "firmware version (default: from model profile)")
	name := flag.String("name", "", "device hostname (default: UBNT)")
	ip := flag.String("ip", "192.168.1.242", "device IP reported to the controller")
	adoptOn := flag.Bool("adopt", adoptEnv.enabled,
		"after the fleet informs, log into the controller and adopt every device (default: env SIM_ADOPT). "+
			"Credentials come from SIM_ADOPT_USERNAME and SIM_ADOPT_PASSWORD (or SIM_ADOPT_PASSWORD_FILE).")
	adoptURL := flag.String("adopt-url", adoptEnv.url,
		"controller API URL for adoption, e.g. https://controller:8443 (default: env SIM_ADOPT_URL). "+
			"This is the API port, not the inform port.")
	adoptSite := flag.String("adopt-site", adoptEnv.site, "controller site to adopt into (default: env SIM_ADOPT_SITE)")
	adoptDialect := flag.String("adopt-dialect", adoptEnv.dialect,
		"controller API dialect, classic or unifios (default: env SIM_ADOPT_DIALECT, else inferred "+
			"from the -adopt-url port: 443 is unifios, anything else classic)")
	adoptTimeout := flag.Duration("adopt-timeout", adoptEnv.timeout,
		"how long the whole adoption may take before it is called a failure (default: env SIM_ADOPT_TIMEOUT)")
	healthcheck := flag.Bool("healthcheck", false,
		"probe readiness and exit: 0 once the fleet is informing, 1 otherwise. Starts nothing and needs "+
			"no other flag or variable; this is what the image's HEALTHCHECK runs (marker file: "+
			"env UNIFI_EMU_READY_FILE, default "+defaultReadyFile+")")
	showVersion := flag.Bool("V", false, "print unifi-emu build version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildVersion)
		return 0
	}
	if *healthcheck {
		return readyProbe(readyFilePath(os.Getenv))
	}
	if adoptEnvErr != nil {
		log.Print(adoptEnvErr)
		return 1
	}

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// Validate adoption before anything starts: a container that informs but
	// silently never adopts looks exactly like one that is merely slow.
	adoptCfg, err := resolveAdopt(adoptSettings{
		enabled:         *adoptOn,
		url:             *adoptURL,
		site:            *adoptSite,
		dialect:         *adoptDialect,
		timeout:         *adoptTimeout,
		enabledExplicit: set["adopt"],
	}, os.Getenv, os.ReadFile)
	if err != nil {
		log.Print(err)
		return 1
	}

	src := fleetSources{
		devicesFile: *devices,
		envInline:   os.Getenv("SIM_DEVICES"),
		devicesJSON: os.Getenv("UNIFI_EMU_DEVICES_JSON"),
		modelsFlag:  *models,
		models:      os.Getenv("SIM_MODELS"),
	}
	specs, ignored, err := fleetSpecs(src, set)
	if err != nil {
		log.Print(err)
		return 1
	}
	// No explicit source: fall back to the image's baked-in default fleet
	// (SIM_DEFAULT_DEVICES), so a bare `docker run` boots a fleet while any
	// explicit source above still wins.
	if specs == nil {
		def, err := defaultFleet(os.Getenv("SIM_DEFAULT_DEVICES"), set)
		if err != nil {
			log.Printf("default fleet: %v", err)
			return 1
		}
		specs = def
	}
	// A device container owns one address — its own, on the Docker network —
	// and every device it runs informs from it. This overwrites before
	// expandFleet so the per-device auto-IP never runs on this path: a
	// synthesized .100+n address is reported to the controller and stored as
	// the device's IP, and then answers nothing for whoever reads it back.
	if strings.TrimSpace(src.devicesJSON) != "" {
		ip, err := containerIPv4(realNet())
		if err != nil {
			log.Printf("container IP: %v", err)
			return 1
		}
		log.Printf("device container: reporting %s for all %d devices", ip, len(specs))
		for i := range specs {
			specs[i].IP = ip
		}
	}
	if specs != nil {
		macBase := envOr("SIM_MAC_BASE", "00:27:22:e0:00:00")
		ipBase := envOr("SIM_IP_BASE", "192.168.1.100")
		specs, err = expandFleet(specs, macBase, ipBase)
		if err != nil {
			log.Printf("expand fleet: %v", err)
			return 1
		}
	}

	resolveCtx, cancelResolve := context.WithTimeout(context.Background(), 5*time.Second)
	resolved, err := emu.ResolveInformURL(resolveCtx, *inform)
	cancelResolve()
	if err != nil {
		log.Printf("resolve inform URL: %v", err)
		return 1
	}
	if resolved != *inform {
		log.Printf("inform URL %s resolved to %s for the reported inform_url", *inform, resolved)
		*inform = resolved
	}
	if specs == nil {
		specs = []emu.DeviceSpec{{
			MAC: *mac, Type: *typ, Model: *model, ModelDisplay: *modelDisplay,
			Version: *version, Name: *name, IP: *ip,
		}}
	} else if len(ignored) > 0 {
		log.Printf("fleet env source set: ignoring single-device flags -%s", strings.Join(ignored, ", -"))
	}

	e := emu.New(*inform)
	if err := e.Add(specs...); err != nil {
		log.Printf("add devices: %v", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := e.Start(ctx); err != nil {
		log.Printf("start: %v", err)
		return 1
	}
	defer e.Stop()
	macs := make([]string, 0, len(specs))
	for _, s := range specs {
		m, _ := e.State(s.MAC)
		log.Printf("[%s] %s at %s informing %s (%s)", s.MAC, s.Model, s.IP, *inform, m)
		go watch(ctx, e, s.MAC)
		macs = append(macs, s.MAC)
	}
	// The marker goes down only now: every device in the fleet has an inform
	// loop running, which is the whole of what "ready" claims. Failing to
	// write it is fatal because the container could never turn healthy, and
	// one that runs forever while reporting unhealthy is worse than one that
	// exits with the reason.
	if err := writeReady(readyFilePath(os.Getenv)); err != nil {
		log.Print(err)
		return 1
	}

	if adoptCfg.enabled {
		// A signal during adoption is an ordinary shutdown, not a failure:
		// the deadline and the signal share this context, so ask which one
		// ended it before calling the run bad.
		if err := runAdopt(ctx, adoptCfg, macs); err != nil {
			if ctx.Err() != nil {
				log.Printf("adopt: interrupted, stopping")
				return 0
			}
			log.Printf("adopt: %v", err)
			return 1
		}
	}

	<-ctx.Done()
	log.Print("signal received, stopping")
	return 0
}

// envOr returns the env var key's value, or def if it is unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// informURLDefault is the -inform default: the device-container variable
// first, then the older SIM_CONTROLLER, then localhost. Both are kept
// because SIM_CONTROLLER is the compose-sidecar contract and predates the
// container one; an explicit -inform beats either by being a flag.
func informURLDefault(getenv func(string) string) string {
	for _, key := range []string{"UNIFI_EMU_INFORM_URL", "SIM_CONTROLLER"} {
		if v := getenv(key); v != "" {
			return v
		}
	}
	return "http://localhost:8080/inform"
}

// defaultReadyFile is where the readiness marker lives unless
// UNIFI_EMU_READY_FILE says otherwise. It sits at the root because the image
// is FROM scratch: there is no /var, no /run and no shell to make one.
const defaultReadyFile = "/unifi-emu-ready"

// readyFilePath resolves the readiness marker path.
func readyFilePath(getenv func(string) string) string {
	if p := getenv("UNIFI_EMU_READY_FILE"); p != "" {
		return p
	}
	return defaultReadyFile
}

// readyProbe is the -healthcheck body: the marker's existence as an exit
// code. It touches nothing else — no flags, no other variable, no network —
// so a failing probe means the fleet is not up rather than that the probe
// itself tripped over something.
func readyProbe(path string) int {
	if _, err := os.Stat(path); err != nil {
		return 1
	}
	return 0
}

// writeReady drops the readiness marker. Its content is nothing: the
// probe asks whether the file is there, and anything written into it would
// be a second thing to keep true.
func writeReady(path string) error {
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return fmt.Errorf("write readiness marker (UNIFI_EMU_READY_FILE): %w", err)
	}
	return nil
}

// watch logs a line whenever mac's adoption state changes, so long runs
// show progress without per-inform noise (the device loop logs those).
func watch(ctx context.Context, e *emu.Emu, mac string) {
	last, ok := e.State(mac)
	if !ok {
		return
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur, ok := e.State(mac)
			if !ok {
				return
			}
			if cur != last {
				log.Printf("[%s] %s -> %s", mac, last, cur)
				last = cur
			}
		}
	}
}
