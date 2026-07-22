// Command unifi-emu runs a fleet of emulated UniFi devices informing a
// real controller, either a single device from flags or a fleet from a
// JSON file, until interrupted.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	unifiemu "github.com/jamesbraid/unifi-emu"
)

func main() {
	informDefault := os.Getenv("SIM_CONTROLLER")
	if informDefault == "" {
		informDefault = "http://localhost:8080/inform"
	}
	inform := flag.String("inform", informDefault, "controller inform URL (default: env SIM_CONTROLLER)")
	devices := flag.String("devices", "", "JSON file with an array of DeviceSpec (fleet mode; "+
		"keys match DeviceSpec field names case-insensitively: mac, type, model, modeldisplay, version, name, ip, ports, ssids)")
	mac := flag.String("mac", "00:27:22:e0:00:01", "device MAC (single-device mode)")
	typ := flag.String("type", "", "device type ugw/usw/uap (default: from model profile)")
	model := flag.String("model", "UGW3", "device model")
	modelDisplay := flag.String("model-display", "", "model display name (default: from model profile)")
	version := flag.String("version", "4.4.36.5146617", "firmware version")
	name := flag.String("name", "", "device hostname (default: UBNT)")
	ip := flag.String("ip", "192.168.1.242", "device IP reported to the controller")
	flag.Parse()

	// -devices is a complete device list; mixing it with single-device
	// flags would silently drop one of the two definitions, so reject it.
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var specs []unifiemu.DeviceSpec
	if *devices != "" {
		for _, f := range []string{"mac", "type", "model", "model-display", "version", "name", "ip"} {
			if set[f] {
				log.Fatalf("-devices cannot be combined with -%s", f)
			}
		}
		b, err := os.ReadFile(*devices)
		if err != nil {
			log.Fatalf("read %s: %v", *devices, err)
		}
		if err := json.Unmarshal(b, &specs); err != nil {
			log.Fatalf("parse %s: %v", *devices, err)
		}
		if len(specs) == 0 {
			log.Fatalf("%s: no devices", *devices)
		}
	} else {
		specs = []unifiemu.DeviceSpec{{
			MAC: *mac, Type: *typ, Model: *model, ModelDisplay: *modelDisplay,
			Version: *version, Name: *name, IP: *ip,
		}}
	}

	e := unifiemu.New(*inform)
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
func watch(ctx context.Context, e *unifiemu.Emu, mac string) {
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
