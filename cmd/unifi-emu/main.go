// Command unifi-emu runs a fleet of emulated UniFi devices informing a
// real controller until interrupted. Device sources (mutually exclusive):
// -devices FILE (YAML/JSON), SIM_DEVICES env (inline YAML), or the
// single-device flags.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jamesbraid/unifi-emu"
)

func main() {
	informDefault := os.Getenv("SIM_CONTROLLER")
	if informDefault == "" {
		informDefault = "http://localhost:8080/inform"
	}
	inform := flag.String("inform", informDefault, "controller inform URL (default: env SIM_CONTROLLER)")
	devices := flag.String("devices", "", "YAML/JSON file with an array of DeviceSpec (fleet mode; "+
		"keys: mac, type, model, modeldisplay, version, name, ip, ports, ssids; unknown keys rejected). "+
		"Fleet sources (mutually exclusive): -devices FILE (YAML/JSON) or SIM_DEVICES env (inline YAML list); either beats single-device flags")
	mac := flag.String("mac", "00:27:22:e0:00:01", "device MAC (single-device mode)")
	typ := flag.String("type", "", "device type ugw/usw/uap (default: from model profile)")
	model := flag.String("model", "UGW3", "device model")
	modelDisplay := flag.String("model-display", "", "model display name (default: from model profile)")
	version := flag.String("version", "", "firmware version (default: from model profile)")
	name := flag.String("name", "", "device hostname (default: UBNT)")
	ip := flag.String("ip", "192.168.1.242", "device IP reported to the controller")
	flag.Parse()

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	specs, ignored, err := fleetSpecs(*devices, os.Getenv("SIM_DEVICES"), set)
	if err != nil {
		log.Fatal(err)
	}
	if specs == nil {
		specs = []emu.DeviceSpec{{
			MAC: *mac, Type: *typ, Model: *model, ModelDisplay: *modelDisplay,
			Version: *version, Name: *name, IP: *ip,
		}}
	} else if len(ignored) > 0 {
		log.Printf("SIM_DEVICES set: ignoring single-device flags -%s", strings.Join(ignored, ", -"))
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
		log.Printf("[%s] %s %s at %s informing %s (%s)", s.MAC, s.Model, s.Version, s.IP, *inform, m)
		go watch(ctx, e, s.MAC)
	}
	<-ctx.Done()
	log.Print("signal received, stopping")
	e.Stop()
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
