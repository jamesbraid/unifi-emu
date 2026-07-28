// Command unifi-emu-herder starts a fleet of fake UniFi devices as Docker
// containers on a network the caller already owns, and reports what it
// started as versioned NDJSON on stdout.
//
// The caller owns the network, the controller, the device request, adoption
// and the assertions. The herder owns device planning and device-container
// lifecycle, nothing else: it never starts a controller, never accepts
// credentials, and never creates or removes the caller's network.
//
//	unifi-emu-herder \
//	  --network unifi-lab \
//	  --inform-url http://172.28.0.2:8080/inform \
//	  --devices -
//
// Stdout carries the control protocol and nothing else. Diagnostics and
// aggregated device logs go to stderr. Exit 0 means a stop signal was handled
// and cleanup succeeded, 1 means a failure after protocol mode began, and 2
// means the command line itself was wrong.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jamesbraid/unifi-emu/internal/herder"
)

// buildVersion and defaultSyntheticImage are stamped at release time via
// -ldflags. A development binary has version "dev" and no image default, so
// it requires --synthetic-image whenever the plan needs a synthetic device;
// neither path falls back to a floating tag.
var (
	buildVersion          = "dev"
	defaultSyntheticImage = ""
)

func main() {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	os.Exit(run(env{
		args:    os.Args[1:],
		stdin:   os.Stdin,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		getenv:  os.Getenv,
		version: buildVersion,
		image:   defaultSyntheticImage,
		backend: herder.NewDockerBackend,
		signals: signals,
	}))
}

// env is the process the command runs in, injected so the flag, exit and
// protocol behaviour can be tested without a Docker daemon.
type env struct {
	args    []string
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	getenv  func(string) string
	version string
	image   string
	backend func(context.Context) (herder.Backend, error)
	signals chan os.Signal
}

type flags struct {
	network        string
	informURL      string
	devices        string
	runtimeConfig  string
	syntheticImage string
	startupTimeout time.Duration
	stopTimeout    time.Duration
	showVersion    bool
}

// errUsage marks a command line the herder refuses before protocol mode.
var errUsage = errors.New("usage")

func parseFlags(args []string, stderr io.Writer) (flags, *flag.FlagSet, error) {
	var f flags
	fs := flag.NewFlagSet("unifi-emu-herder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.network, "network", "", "existing Docker network to attach every device container to (required)")
	fs.StringVar(&f.informURL, "inform-url", "",
		"controller inform endpoint, exactly http://<IPv4>:<port>/inform (required)")
	fs.StringVar(&f.devices, "devices", "",
		"device request: a JSON file, or - to read one request from stdin (required)")
	fs.StringVar(&f.runtimeConfig, "runtime-config", "",
		"runner-local runtime mapping file (default: $UNIFI_EMU_RUNTIME_CONFIG, "+
			"else /etc/unifi-emu-herder/runtimes.json when it exists, else no mappings)")
	fs.StringVar(&f.syntheticImage, "synthetic-image", "",
		"override the public synthetic image (development builds have no default)")
	fs.DurationVar(&f.startupTimeout, "startup-timeout", 5*time.Minute,
		"how long validation, pulls, container start and readiness may take in total")
	fs.DurationVar(&f.stopTimeout, "stop-timeout", 30*time.Second,
		"how long a device container may take to stop before it is force-removed")
	fs.BoolVar(&f.showVersion, "version", false, "print the herder version and exit")
	if err := fs.Parse(args); err != nil {
		return f, fs, errUsage
	}
	return f, fs, nil
}

func run(e env) int {
	f, fs, err := parseFlags(e.args, e.stderr)
	if err != nil {
		return herder.ExitUsage
	}
	if f.showVersion {
		fmt.Fprintln(e.stdout, e.version) //nolint:errcheck // a closed stdout is reported by the exit status
		return herder.ExitOK
	}
	// The command has no subcommands, so a bare word is a mistyped flag.
	if fs.NArg() > 0 {
		return usage(e, fs, "unexpected argument %q", fs.Arg(0))
	}
	for _, required := range []struct{ name, value string }{
		{"network", f.network}, {"inform-url", f.informURL}, {"devices", f.devices},
	} {
		if required.value == "" {
			return usage(e, fs, "--%s is required", required.name)
		}
	}

	// Everything from here on is protocol mode: failures are reported as
	// NDJSON and exit 1, never as usage.
	runID, err := herder.NewRunID()
	if err != nil {
		fmt.Fprintf(e.stderr, "unifi-emu-herder: %v\n", err) //nolint:errcheck // stderr is best effort
		return herder.ExitFailure
	}
	request, closeRequest, err := openRequest(f.devices, e.stdin)
	if err != nil {
		// The file is part of the request, so this is reported through the
		// protocol rather than as a usage error -- but only the herder can
		// emit that, so hand it the failure as an unreadable request.
		request = failedReader{err}
	}
	defer closeRequest()

	image, err := herder.ResolveSyntheticImage(f.syntheticImage, e.image)
	if err != nil {
		fmt.Fprintf(e.stderr, "unifi-emu-herder: %v\n", err) //nolint:errcheck // stderr is best effort
		return herder.ExitFailure
	}

	return herder.Run(context.Background(), herder.Options{
		Network:           f.network,
		InformURL:         f.informURL,
		Request:           request,
		RuntimeConfigPath: f.runtimeConfig,
		SyntheticImage:    image,
		StartupTimeout:    f.startupTimeout,
		StopTimeout:       f.stopTimeout,
		Stdout:            e.stdout,
		Stderr:            e.stderr,
		Getenv:            e.getenv,
		RunID:             runID,
		NewBackend:        e.backend,
		Signals:           e.signals,
	})
}

func usage(e env, fs *flag.FlagSet, format string, args ...any) int {
	fmt.Fprintf(e.stderr, "unifi-emu-herder: "+format+"\n", args...) //nolint:errcheck // stderr is best effort
	fs.Usage()
	return herder.ExitUsage
}

// openRequest resolves --devices to the reader carrying one request document.
// "-" reads stdin, which ends at EOF and has no control meaning afterwards.
func openRequest(path string, stdin io.Reader) (io.Reader, func(), error) {
	if path == "-" {
		return stdin, func() {}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, func() {}, err
	}
	return file, func() { _ = file.Close() }, nil
}

// failedReader turns an unreadable request file into a decode failure, so it
// is reported as invalid_request after `started` rather than as CLI usage.
type failedReader struct{ err error }

func (f failedReader) Read([]byte) (int, error) { return 0, f.err }
