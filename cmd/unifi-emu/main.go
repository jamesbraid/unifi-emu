// Command unifi-emu runs a fleet of emulated UniFi devices informing a
// real controller until interrupted. Device sources (mutually exclusive):
// -devices FILE (YAML/JSON), SIM_DEVICES env (inline YAML), -models
// (terse MODEL[:count] list), SIM_MODELS env, or the single-device flags.
// When none of those is set, SIM_DEFAULT_DEVICES (the image's baked-in
// fleet file) is used if present; any explicit source overrides it.
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
	informDefault := os.Getenv("SIM_CONTROLLER")
	if informDefault == "" {
		informDefault = "http://localhost:8080/inform"
	}
	inform := flag.String("inform", informDefault, "controller inform URL (default: env SIM_CONTROLLER)")
	devices := flag.String("devices", "", "YAML/JSON file with an array of DeviceSpec (fleet mode; "+
		"keys: mac, type, model, modeldisplay, version, name, ip, ports, ssids; unknown keys rejected). "+
		"Fleet sources (mutually exclusive): -devices FILE (YAML/JSON), SIM_DEVICES env (inline YAML list), "+
		"-models, or SIM_MODELS env; any one beats single-device flags")
	models := flag.String("models", "", "terse fleet: comma-separated MODEL[:count] (e.g. U7PRO,USM8P:2,UGW3); "+
		"MAC/IP auto-derived from SIM_MAC_BASE/SIM_IP_BASE. Fleet sources are mutually exclusive.")
	mac := flag.String("mac", "00:27:22:e0:00:01", "device MAC (single-device mode)")
	typ := flag.String("type", "", "device type ugw/usw/uap (default: from model profile)")
	model := flag.String("model", "UGW3", "device model")
	modelDisplay := flag.String("model-display", "", "model display name (default: from model profile)")
	version := flag.String("version", "", "firmware version (default: from model profile)")
	name := flag.String("name", "", "device hostname (default: UBNT)")
	ip := flag.String("ip", "192.168.1.242", "device IP reported to the controller")
	showVersion := flag.Bool("V", false, "print unifi-emu build version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildVersion)
		return
	}

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	src := fleetSources{
		devicesFile: *devices,
		envInline:   os.Getenv("SIM_DEVICES"),
		modelsFlag:  *models,
		models:      os.Getenv("SIM_MODELS"),
	}
	specs, ignored, err := fleetSpecs(src, set)
	if err != nil {
		log.Fatal(err)
	}
	// No explicit source: fall back to the image's baked-in default fleet
	// (SIM_DEFAULT_DEVICES), so a bare `docker run` boots a fleet while any
	// explicit source above still wins.
	if specs == nil {
		def, err := defaultFleet(os.Getenv("SIM_DEFAULT_DEVICES"), set)
		if err != nil {
			log.Fatalf("default fleet: %v", err)
		}
		specs = def
	}
	if specs != nil {
		macBase := envOr("SIM_MAC_BASE", "00:27:22:e0:00:00")
		ipBase := envOr("SIM_IP_BASE", "192.168.1.100")
		specs, err = expandFleet(specs, macBase, ipBase)
		if err != nil {
			log.Fatalf("expand fleet: %v", err)
		}
	}

	resolveCtx, cancelResolve := context.WithTimeout(context.Background(), 5*time.Second)
	resolved, err := emu.ResolveInformURL(resolveCtx, *inform)
	cancelResolve()
	if err != nil {
		log.Fatalf("resolve inform URL: %v", err)
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
		log.Fatalf("add devices: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := e.Start(ctx); err != nil {
		log.Fatalf("start: %v", err)
	}
	for _, s := range specs {
		m, _ := e.State(s.MAC)
		log.Printf("[%s] %s at %s informing %s (%s)", s.MAC, s.Model, s.IP, *inform, m)
		go watch(ctx, e, s.MAC)
	}
	<-ctx.Done()
	log.Print("signal received, stopping")
	e.Stop()
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
