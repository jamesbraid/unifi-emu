// Command unifi-emu runs a fleet of emulated UniFi devices informing a
// real controller until interrupted. Device sources (mutually exclusive):
// -devices FILE (YAML/JSON), SIM_DEVICES env (inline YAML), or the
// single-device flags.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
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
		"Fleet sources (mutually exclusive): -devices FILE (YAML/JSON) or SIM_DEVICES env (inline YAML list); either beats single-device flags")
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
	specs, ignored, err := fleetSpecs(*devices, os.Getenv("SIM_DEVICES"), set)
	if err != nil {
		log.Fatal(err)
	}

	if resolved := resolveInformURL(*inform); resolved != *inform {
		log.Printf("inform URL %s resolved to %s for the reported inform_url", *inform, resolved)
		*inform = resolved
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
		log.Printf("[%s] %s at %s informing %s (%s)", s.MAC, s.Model, s.IP, *inform, m)
		go watch(ctx, e, s.MAC)
	}
	<-ctx.Done()
	log.Print("signal received, stopping")
	e.Stop()
}

// resolveInformURL rewrites raw's host to its resolved IPv4 address.
// Controllers validate the inform_url a device reports and recent
// versions reject informs whose host is not an IP they recognize
// ("invalid inform_ip <host>"), which deadlocks adoption when the sim
// dials a compose DNS name such as http://unifi:8080/inform. Dialing the
// resolved IP is equivalent and reports an inform_url the controller
// accepts. IPs and unresolvable hosts are returned unchanged.
func resolveInformURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return raw
	}
	// Bound the lookup: a hanging resolver must not stall startup.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		// Pass through unresolved: per-inform dials re-resolve lazily,
		// but say so — a persistent failure shows up as the controller
		// rejecting a hostname inform_ip post-adoption.
		log.Printf("could not resolve inform host %q to IPv4: %v; using it as-is", host, err)
		return raw
	}
	if len(ips) == 0 {
		log.Printf("inform host %q has no IPv4 address; using it as-is", host)
		return raw
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(ips[0].String(), port)
	} else {
		u.Host = ips[0].String()
	}
	return u.String()
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
