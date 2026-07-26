package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jamesbraid/unifi-emu/internal/adopt"
)

// defaultAdoptTimeout bounds the whole adoption run. It is generous on
// purpose: adoption failing is fatal to the container, so the deadline must
// only ever fire on a genuine failure, never on a controller having a slow
// morning.
const defaultAdoptTimeout = 5 * time.Minute

// adoptSettings is the -adopt* flag surface. Each field is pre-filled from
// the matching SIM_ADOPT_* variable so an explicit flag beats the
// environment and both beat the built-in default — the same layering
// -inform already uses with SIM_CONTROLLER.
type adoptSettings struct {
	enabled bool
	url     string
	site    string
	dialect string
	timeout time.Duration

	// enabledExplicit records that -adopt was given on the command line,
	// whichever way it was set. It only matters when adoption is off: it
	// separates a deliberate -adopt=false from forgetting the switch.
	enabledExplicit bool
}

// adoptConfig is a validated configuration, ready to drive adoption.
type adoptConfig struct {
	enabled  bool
	url      string
	site     string
	dialect  adopt.Dialect
	username string
	password string
	timeout  time.Duration

	// dialectFromPort records that the dialect was guessed from the URL's
	// port rather than stated. The guess is a heuristic, so the caller logs
	// which way it went; a wrong guess is otherwise diagnosed only by a
	// baffling login failure.
	dialectFromPort bool
}

// adoptEnvDefaults reads the SIM_ADOPT_* environment into the flag defaults.
func adoptEnvDefaults(getenv func(string) string) (adoptSettings, error) {
	s := adoptSettings{
		url:     getenv("SIM_ADOPT_URL"),
		site:    getenv("SIM_ADOPT_SITE"),
		dialect: getenv("SIM_ADOPT_DIALECT"),
		timeout: defaultAdoptTimeout,
	}
	if s.site == "" {
		s.site = "default"
	}
	if raw := getenv("SIM_ADOPT"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return adoptSettings{}, fmt.Errorf("SIM_ADOPT %q: want a boolean (1/0, true/false)", raw)
		}
		s.enabled = enabled
	}
	if raw := getenv("SIM_ADOPT_TIMEOUT"); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil {
			return adoptSettings{}, fmt.Errorf("SIM_ADOPT_TIMEOUT %q: %w", raw, err)
		}
		s.timeout = timeout
	}
	return s, nil
}

// resolveAdopt validates settings and folds in the credentials, which are
// environment-only: a password on the command line would show up in every
// process listing.
//
// The password comes from SIM_ADOPT_PASSWORD, or from the file named by
// SIM_ADOPT_PASSWORD_FILE when that is set, so CI can mount a secret instead
// of putting it in the workflow. Every problem here is fatal before the
// fleet starts: a container that informs but silently never adopts is
// indistinguishable from one that is merely slow.
func resolveAdopt(
	s adoptSettings,
	getenv func(string) string,
	readFile func(string) ([]byte, error),
) (adoptConfig, error) {
	username := getenv("SIM_ADOPT_USERNAME")
	passwordFile := getenv("SIM_ADOPT_PASSWORD_FILE")
	password := getenv("SIM_ADOPT_PASSWORD")

	if !s.enabled {
		// Settings without the switch mean someone expected adoption and
		// will not get it. Say so rather than quietly informing forever.
		// An explicit -adopt=false is not that mistake: it is how a
		// command line turns off what the environment asked for.
		settingsPresent := s.url != "" || s.dialect != "" ||
			username != "" || password != "" || passwordFile != ""
		if settingsPresent && !s.enabledExplicit {
			return adoptConfig{}, fmt.Errorf(
				"adopt settings are present but adoption is off; pass -adopt or set SIM_ADOPT=1")
		}
		return adoptConfig{}, nil
	}

	if s.url == "" {
		return adoptConfig{}, fmt.Errorf("adoption needs the controller API URL: set -adopt-url or SIM_ADOPT_URL")
	}
	if username == "" {
		return adoptConfig{}, fmt.Errorf("adoption needs a controller login: set SIM_ADOPT_USERNAME")
	}
	if passwordFile != "" {
		body, err := readFile(passwordFile)
		if err != nil {
			return adoptConfig{}, fmt.Errorf("read SIM_ADOPT_PASSWORD_FILE %s: %w", passwordFile, err)
		}
		// Trailing newlines come free with every editor and shell redirect
		// and are not part of the password.
		password = strings.TrimRight(string(body), "\r\n")
	}
	if password == "" {
		return adoptConfig{}, fmt.Errorf(
			"adoption needs a controller password: set SIM_ADOPT_PASSWORD or SIM_ADOPT_PASSWORD_FILE")
	}
	// A zero budget expires the instant adoption starts, reporting a healthy
	// fleet as one that never adopted. There is no "no deadline" option:
	// failing to adopt has to stay fatal for the container to be useful.
	if s.timeout <= 0 {
		return adoptConfig{}, fmt.Errorf(
			"-adopt-timeout must be positive, got %s (SIM_ADOPT_TIMEOUT sets it too)", s.timeout)
	}

	cfg := adoptConfig{
		enabled:  true,
		url:      strings.TrimRight(s.url, "/"),
		site:     s.site,
		username: username,
		password: password,
		timeout:  s.timeout,
	}
	if cfg.site == "" {
		cfg.site = "default"
	}
	if s.dialect == "" {
		cfg.dialect = adopt.DialectForURL(cfg.url)
		cfg.dialectFromPort = true
	} else {
		dialect, err := adopt.ParseDialect(s.dialect)
		if err != nil {
			return adoptConfig{}, err
		}
		cfg.dialect = dialect
	}
	return cfg, nil
}

// diagnosisTimeout bounds the post-mortem stat/device reads. It runs off a
// fresh context because the run's own deadline has usually just expired,
// which is exactly when the diagnosis matters most.
const diagnosisTimeout = 15 * time.Second

// runAdopt logs into the controller and drives every MAC to connected. It
// returns once the whole fleet is adopted, or with the first failure.
//
// The devices are already informing by the time this runs: adoption is a
// controller-side command against a device the controller has to have seen,
// which is why this cannot happen before the fleet starts.
func runAdopt(parent context.Context, cfg adoptConfig, macs []string) error {
	if cfg.dialectFromPort {
		log.Printf("adopt: %s dialect inferred from the URL port; -adopt-dialect overrides", cfg.dialect)
	}
	log.Printf("adopt: %s site %s as %s (%s dialect, timeout %s)",
		cfg.url, cfg.site, cfg.username, cfg.dialect, cfg.timeout)

	ctx, cancel := context.WithTimeout(parent, cfg.timeout)
	defer cancel()

	client := adopt.New(cfg.url, cfg.dialect)
	client.OnWaiting = func(detail string) {
		log.Printf("adopt: controller not ready yet, waiting: %s", detail)
	}
	if err := client.Login(ctx, cfg.username, cfg.password); err != nil {
		return fmt.Errorf("controller login: %w", err)
	}
	err := adopt.Fleet(ctx, client, cfg.site, macs, adopt.Options{
		OnPending:   func(d adopt.Device) { log.Printf("adopt: %s pending on the controller", d.MAC) },
		OnConnected: func(d adopt.Device) { log.Printf("adopt: %s connected (model %s)", d.MAC, d.Model) },
	})
	if err != nil {
		// When the failure is the interrupt, nobody is going to read a
		// post-mortem and its round trips can push shutdown past the stop
		// grace period. Diagnose real failures only.
		if parent.Err() == nil {
			logAdoptDiagnosis(cfg, client, macs)
		}
		return err
	}
	log.Printf("adopt: all %d devices connected", len(macs))
	return nil
}

// logAdoptDiagnosis reports what the controller thinks of every device after
// a failed run. Without it the operator sees one device's error and has to
// guess whether the rest got anywhere — and the container is about to exit,
// taking the chance to look with it.
func logAdoptDiagnosis(cfg adoptConfig, client *adopt.Client, macs []string) {
	ctx, cancel := context.WithTimeout(context.Background(), diagnosisTimeout)
	defer cancel()
	for _, mac := range macs {
		device, err := client.DeviceByMAC(ctx, cfg.site, mac)
		if err != nil {
			log.Printf("adopt: %s final controller state unknown: %v", mac, err)
			continue
		}
		log.Printf("adopt: %s final controller state=%d adopted=%v model=%s",
			mac, device.State, device.Adopted, device.Model)
	}
}
