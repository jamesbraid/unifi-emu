// Command unifi-emu runs a fleet of emulated UniFi devices informing a
// real controller until interrupted. Device sources (mutually exclusive):
// -devices FILE (YAML/JSON), SIM_DEVICES env (inline YAML), -models
// (terse MODEL[:count] list), SIM_MODELS env, or the single-device flags.
// When none of those is set, SIM_DEFAULT_DEVICES (the image's baked-in
// fleet file) is used if present; any explicit source overrides it.
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
	informDefault := os.Getenv("UNIFI_EMU_INFORM_URL")
	if informDefault == "" {
		informDefault = os.Getenv("SIM_CONTROLLER")
	}
	if informDefault == "" {
		informDefault = "http://localhost:8080/inform"
	}
	adoptEnv, err := adoptEnvDefaults(os.Getenv)
	if err != nil {
		log.Print(err)
		return 1
	}
	inform := flag.String("inform", informDefault, "controller inform URL (default: env UNIFI_EMU_INFORM_URL or SIM_CONTROLLER)")
	devices := flag.String("devices", "", "YAML/JSON file with an array of DeviceSpec (fleet mode; "+
		"keys: mac, type, model, modeldisplay, version, name, ip, ports, ssids, serial; unknown keys rejected). "+
		"Fleet sources (mutually exclusive): -devices FILE (YAML/JSON), SIM_DEVICES env (inline YAML list), "+
		"-models, SIM_MODELS env, or UNIFI_EMU_DEVICES_JSON env; any one beats single-device flags")
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
	showVersion := flag.Bool("V", false, "print unifi-emu build version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildVersion)
		return 0
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
		modelsFlag:  *models,
		models:      os.Getenv("SIM_MODELS"),
		devicesJSON: os.Getenv("UNIFI_EMU_DEVICES_JSON"),
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
