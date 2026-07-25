//go:build integration

package emu_test

// The catalog sweep answers a question the gate cannot: of the models the
// emulator claims to support, which ones does a real controller actually
// accept? The random-fleet gate draws from a curated pool of six because only
// those have ever been measured; everything else is believed, not known.
//
// Two tiers, because the two failure modes cost very different amounts. The
// registration tier only asks whether the controller ever lists the device,
// which needs no adoption and can run a whole batch per boot. The adoption
// tier drives each device to CONNECTED one at a time, which is minutes per
// model but is the only thing that proves a model is gate-worthy.
//
// Both are opt-in. They boot many controllers and take tens of minutes, so
// they must never run as part of the PR gate.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/testcontainers/testcontainers-go"
)

const (
	sweepEnableEnv   = "UNIFI_EMU_ITEST_SWEEP"
	sweepBatchEnv    = "UNIFI_EMU_ITEST_SWEEP_BATCH"
	sweepParallelEnv = "UNIFI_EMU_ITEST_SWEEP_PARALLEL"
	sweepModelsEnv   = "UNIFI_EMU_ITEST_SWEEP_MODELS"
	sweepSite        = "default"

	// sweepSettle is how long a batch gets for every device to appear. A
	// device informs every 10s, so this is many attempts, not one: the
	// question is whether the controller ever creates the document, not
	// whether it does so promptly.
	sweepSettle = 3 * time.Minute
	sweepPoll   = 5 * time.Second

	// sweepRetestLimit caps the solo re-test pass. A controller that failed
	// every model would otherwise re-boot once per model and run for hours;
	// what is truncated is always logged.
	sweepRetestLimit = 24
)

// TestClassicCatalogSweepLive checks that every model in the catalog reaches a
// controller document. This is the cheap tier and the one that finds models
// like UGWHD4, which inform normally and are silently never listed.
func TestClassicCatalogSweepLive(t *testing.T) {
	runCatalogSweep(t, "registration", 24)
}

// TestClassicCatalogAdoptionSweepLive drives every model all the way to
// CONNECTED. Registering only proves the controller built a pending document;
// a model can do that and still fail the set-adopt handshake, and this is the
// tier that would catch it. Smaller batches than the registration tier because
// adoption runs serially inside a batch, so batch size is wall-clock.
func TestClassicCatalogAdoptionSweepLive(t *testing.T) {
	runCatalogSweep(t, "adoption", 12)
}

// requireSweepEnabled skips unless the operator asked for a sweep.
func requireSweepEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv(sweepEnableEnv) == "" {
		t.Skipf("catalog sweep is opt-in: set %s=1 (boots many controllers, takes tens of minutes)",
			sweepEnableEnv)
	}
}

// buildEmulatorImageOnce builds the emulator image a single time for a whole
// sweep and returns the image to run.
//
// Building per batch, which is what starting a harness does by default, makes a
// sweep depend on the working tree staying unchanged for its entire run: a
// sweep spans tens of minutes, and a rebuild partway through picks up whatever
// the checkout holds then. Editing the catalog mid-run therefore breaks later
// batches with "unknown model", which reads as a controller fault and is not
// one. Building up front pins every batch to the tree the run started with.
//
// An operator-supplied image is honoured as-is; nothing is built in that case.
func buildEmulatorImageOnce(t *testing.T) string {
	t.Helper()
	if prebuilt := loadITestImages().emulator; prebuilt != "" {
		t.Logf("emulator image: using prebuilt %s", prebuilt)
		return prebuilt
	}

	// A run-unique tag: two sweeps on one host must not overwrite each other's
	// image, which would reintroduce exactly the cross-run bleed this avoids.
	tag := fmt.Sprintf("sweep-%d", time.Now().UnixNano())
	request := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    ".",
			Dockerfile: "Dockerfile",
			Repo:       "unifi-emu-itest",
			Tag:        tag,
			BuildArgs:  emulatorBuildArgs(runtime.GOARCH),
		},
	}

	// Normally the first harness does this, but the prebuild runs before any
	// harness exists and the provider cannot find a rootless or Colima socket
	// without it.
	configureContainerRuntime(t)

	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		t.Fatalf("connect to the container runtime: %v", err)
	}
	defer func() { _ = provider.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	image, err := provider.BuildImage(ctx, &request)
	if err != nil {
		t.Fatalf("build the emulator image: %v", err)
	}
	t.Logf("emulator image: built %s once for every batch", image)
	return image
}

// adoptionExceptions are models a controller deliberately refuses to adopt for
// a reason no emulated device can satisfy, mapped to the refusal it returns.
// They register normally, so the registration tier still covers them; only the
// adoption tier skips them, and it says so rather than quietly shrinking its
// scope. A sweep that stays red on a rule the controller is right to enforce
// gets ignored, and then it stops catching real regressions.
var adoptionExceptions = map[string]string{
	"ULTE": "api.err.LteDeviceAdoptingUnregistered: the controller will not adopt a " +
		"U-LTE that is not registered to an LTE service account. Measured: it " +
		"registers, then the adopt command returns HTTP 400. ULTEPEU and ULTEPUS " +
		"carry no such rule and adopt normally.",
}

// withoutAdoptionExceptions drops the models the controller refuses on
// principle, naming each one and why.
func withoutAdoptionExceptions(t *testing.T, models []string) []string {
	t.Helper()
	kept := make([]string, 0, len(models))
	for _, model := range models {
		if reason, ok := adoptionExceptions[model]; ok {
			t.Logf("not adoption-swept: %s -- %s", model, reason)
			continue
		}
		kept = append(kept, model)
	}
	return kept
}

// sweepModels is the catalog, or the subset named by sweepModelsEnv.
//
// A full adoption sweep runs for over an hour, and anything that interrupts it
// -- a stopped job, a reboot, a laptop lid -- throws away every model measured
// so far, because the report is only written at the end. Naming the models that
// were missed turns a lost run into a short one. It is also how a single
// suspect gets re-measured without paying for the other 181.
func sweepModels(t *testing.T) []string {
	t.Helper()
	all := emu.Models()
	raw := os.Getenv(sweepModelsEnv)
	if strings.TrimSpace(raw) == "" {
		return all
	}
	known := make(map[string]bool, len(all))
	for _, model := range all {
		known[model] = true
	}
	var models []string
	for _, field := range strings.Split(raw, ",") {
		model := strings.TrimSpace(field)
		if model == "" {
			continue
		}
		// An unknown name here is a typo or a model that has since been
		// excluded, and silently sweeping 0 models would look like success.
		if !known[model] {
			t.Fatalf("%s names %q, which is not in the catalog", sweepModelsEnv, model)
		}
		models = append(models, model)
	}
	if len(models) == 0 {
		t.Fatalf("%s=%q names no models", sweepModelsEnv, raw)
	}
	sort.Strings(models)
	return models
}

// sweepSetting reads a positive integer knob, falling back to def.
func sweepSetting(t *testing.T, env string, def int) int {
	t.Helper()
	raw := os.Getenv(env)
	if raw == "" {
		return def
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		t.Fatalf("%s=%q: want a positive integer", env, raw)
	}
	return value
}

func runCatalogSweep(t *testing.T, tier string, defaultBatch int) {
	requireSweepEnabled(t)

	typeOf := func(model string) (string, bool) {
		profile, ok := emu.Profile(model)
		return profile.Type, ok
	}
	models := sweepModels(t)
	if tier == "adoption" {
		models = withoutAdoptionExceptions(t, models)
	}
	// Sweeping nothing would report "0/0, no failures" and pass, which is the
	// most misleading outcome available.
	if len(models) == 0 {
		t.Fatalf("no models left to sweep: every model asked for is an adoption exception")
	}
	batchSize := sweepSetting(t, sweepBatchEnv, defaultBatch)
	// Each controller is a JVM holding a database, so parallelism is bounded
	// by memory on the docker host, not by CPU. Two is safe on a 16GB VM.
	parallel := sweepSetting(t, sweepParallelEnv, 2)

	batches, err := planSweep(models, typeOf, batchSize)
	if err != nil {
		t.Fatalf("plan sweep: %v", err)
	}
	t.Logf("%s sweep: %d models, %d batches of up to %d, %d controllers at a time",
		tier, len(models), len(batches), batchSize, parallel)

	// Built before any batch starts, so every batch runs the same emulator.
	emulatorImage := buildEmulatorImageOnce(t)

	var mu sync.Mutex
	var outcomes []sweepOutcome
	slots := make(chan struct{}, parallel)

	// The inner parallel subtests all finish before this t.Run returns, which
	// is what makes it safe to read outcomes afterwards.
	t.Run("batches", func(t *testing.T) {
		for _, batch := range batches {
			t.Run(fmt.Sprintf("batch%02d", batch.Index), func(t *testing.T) {
				t.Parallel()
				slots <- struct{}{}
				defer func() { <-slots }()

				got := runSweepBatch(t, tier, batch, typeOf, emulatorImage)
				mu.Lock()
				outcomes = append(outcomes, got...)
				mu.Unlock()
			})
		}
	})

	outcomes = confirmSweepFailures(t, tier, outcomes, emulatorImage)

	report := newSweepReport(tier, outcomes)
	path := filepath.Join(evidenceDir(t.Name()), "sweep-report.json")
	if err := writeJSON(path, report); err != nil {
		t.Errorf("write sweep report: %v", err)
	}
	t.Logf("%s", report.summary())
	t.Logf("full report: %s", path)

	if failed := report.failedModels(); len(failed) > 0 {
		t.Fatalf("%d models did not clear the %s bar: %v\nEach was re-tested alone, so these are "+
			"candidates for cmd/modelgen's excludedModels (registration) or simply unfit for the "+
			"gate pool (adoption). Read %s before excluding anything.",
			len(failed), tier, failed, path)
	}
}

// runSweepBatch boots one controller, informs every model in the batch at it,
// and reports what the controller did with each.
func runSweepBatch(
	t *testing.T,
	tier string,
	batch sweepBatch,
	typeOf func(string) (string, bool),
	emulatorImage string,
) []sweepOutcome {
	macs, err := macsForModels(itestMACBase, len(batch.Models))
	if err != nil {
		t.Fatalf("compute MACs for batch %d: %v", batch.Index, err)
	}

	outcomes := make([]sweepOutcome, len(batch.Models))
	for i, model := range batch.Models {
		modelType, _ := typeOf(model)
		outcomes[i] = sweepOutcome{Model: model, Type: modelType, MAC: macs[i], Batch: batch.Index}
	}

	h := startClassicHarness(t)
	h.emulatorImage = emulatorImage
	h.startEmulatorModels(strings.Join(batch.Models, ","))
	client := newControllerClient(h.apiURL, classic)
	if err := client.Login(h.ctx, "admin", "admin"); err != nil {
		t.Fatalf("login: %v", err)
	}

	markRegistered(t, h, client, outcomes, sweepSettle)
	for i := range outcomes {
		if !outcomes[i].Registered {
			outcomes[i].Note = fmt.Sprintf("never listed within %s alongside %d other models",
				sweepSettle, len(batch.Models)-1)
		}
	}

	if tier == "adoption" {
		// Serial: this controller build rejects bursts, which is why the
		// existing fleet test adopts one device at a time too.
		for i := range outcomes {
			if !outcomes[i].Registered {
				continue
			}
			device, err := adoptAndWait(t, h.ctx, h, client, outcomes[i].Model, outcomes[i].MAC)
			if err != nil {
				outcomes[i].Note = err.Error()
				continue
			}
			outcomes[i].Adopted = device.State == 1 && device.Adopted
		}
	}
	return outcomes
}

// markRegistered polls the controller's device list and flags each outcome
// whose MAC appears. It reads the whole list rather than asking per MAC: one
// request covers the batch, and it also shows what else the controller listed.
func markRegistered(
	t *testing.T,
	h *itestHarness,
	client adopter,
	outcomes []sweepOutcome,
	settle time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(settle)
	for {
		missing := 0
		list, err := client.Devices(h.ctx, sweepSite)
		if err != nil {
			t.Logf("stat/device: %v", err)
			missing = len(outcomes)
		} else {
			listed := make(map[string]bool, len(list))
			for _, device := range list {
				listed[strings.ToLower(device.MAC)] = true
			}
			for i := range outcomes {
				if listed[strings.ToLower(outcomes[i].MAC)] {
					outcomes[i].Registered = true
				}
				if !outcomes[i].Registered {
					missing++
				}
			}
		}
		if missing == 0 || !time.Now().Before(deadline) {
			return
		}
		h.requireEmulatorRunning()
		time.Sleep(sweepPoll)
	}
}

// confirmSweepFailures re-tests every failed model on its own controller.
//
// A model can miss in a batch for reasons that say nothing about the model:
// the controller throttling a burst of informs, or a co-tenant taking the
// site's only gateway slot. Booting the suspect alone removes both. Only a
// model that fails twice, the second time with nothing else present, is
// evidence about the model itself -- the same paired-then-solo discipline that
// established UGWHD4.
func confirmSweepFailures(
	t *testing.T,
	tier string,
	outcomes []sweepOutcome,
	emulatorImage string,
) []sweepOutcome {
	byModel := make(map[string]int, len(outcomes))
	var suspects []string
	for i, outcome := range outcomes {
		byModel[outcome.Model] = i
		cleared := outcome.Registered
		if tier == "adoption" {
			cleared = cleared && outcome.Adopted
		}
		if !cleared {
			suspects = append(suspects, outcome.Model)
		}
	}
	if len(suspects) == 0 {
		return outcomes
	}

	if len(suspects) > sweepRetestLimit {
		t.Logf("WARNING: %d models failed; re-testing only the first %d alone. "+
			"The remainder keep their batch result and are NOT confirmed: %v",
			len(suspects), sweepRetestLimit, suspects[sweepRetestLimit:])
		suspects = suspects[:sweepRetestLimit]
	}
	t.Logf("re-testing %d failed models alone to rule out batch effects: %v", len(suspects), suspects)

	t.Run("confirm", func(t *testing.T) {
		for _, model := range suspects {
			t.Run(model, func(t *testing.T) {
				index := byModel[model]
				outcome := outcomes[index]

				h := startClassicHarness(t)
				h.emulatorImage = emulatorImage
				h.startEmulatorModels(model)
				client := newControllerClient(h.apiURL, classic)
				if err := client.Login(h.ctx, "admin", "admin"); err != nil {
					t.Fatalf("login: %v", err)
				}

				// Sole device, so it takes index 0's address.
				macs, err := macsForModels(itestMACBase, 1)
				if err != nil {
					t.Fatalf("compute MAC: %v", err)
				}
				solo := []sweepOutcome{{Model: model, Type: outcome.Type, MAC: macs[0], Batch: -1}}
				markRegistered(t, h, client, solo, sweepSettle)

				if !solo[0].Registered {
					outcome.Registered = false
					outcome.Note = fmt.Sprintf("never listed in %s, alone on a fresh controller "+
						"(also missed in batch %d)", sweepSettle, outcome.Batch)
					outcomes[index] = outcome
					return
				}

				// It registered alone, so the batch result was a batch effect.
				outcome.Registered = true
				outcome.Note = fmt.Sprintf("missed in batch %d but registered alone: batch effect, "+
					"not a model fault", outcome.Batch)
				if tier == "adoption" {
					device, err := adoptAndWait(t, h.ctx, h, client, model, solo[0].MAC)
					if err != nil {
						outcome.Note = fmt.Sprintf("registered alone but did not adopt: %v", err)
					} else {
						outcome.Adopted = device.State == 1 && device.Adopted
					}
				}
				outcomes[index] = outcome
			})
		}
	})
	return outcomes
}
