package emu_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	emu "github.com/jamesbraid/unifi-emu"
)

// sweepBatch is one controller boot's worth of models.
type sweepBatch struct {
	Index  int
	Models []string
}

// planSweep splits the model catalog into batches, one per controller boot,
// for a live sweep that checks whether every model registers. Batches are
// built from the input order alone — no maps, no randomness — so the same
// catalog always yields the same plan and a failing batch is re-runnable by
// index. typeOf reports a model's device type ("ugw"/"uxg"/"usw"/"uap");
// a model it does not know is an error rather than a silent skip, because a
// dropped model is a hole in the sweep's coverage.
//
// Gateways are the scarce resource: a site adopts one gateway, and
// expandFleet rejects a fleet holding two (api.err.NoSecondGateway), so a
// batch carrying two never boots. The registry has roughly nine gateways
// among 186 models, so they are spread one per batch and the rest of each
// batch is packed with non-gateways — otherwise the sweep would waste nine
// boots on single-device fleets. That puts the batch count at
// max(gateways, ceil(models/maxPerBatch)).
func planSweep(models []string, typeOf func(string) (string, bool), maxPerBatch int) ([]sweepBatch, error) {
	// A batch's devices are addressed from itestIPBase upward and nextIP
	// refuses to leave the base /24 past .254. That window (.100-.254) is
	// 155 wide; the cap is 154 so a batch never depends on the last usable
	// address in the /24.
	const maxSweepBatch = 154

	if maxPerBatch < 1 {
		return nil, fmt.Errorf("maxPerBatch %d: want at least 1", maxPerBatch)
	}
	if maxPerBatch > maxSweepBatch {
		return nil, fmt.Errorf(
			"maxPerBatch %d: the sweep harness assigns IPs from %s upward and auto-IP stops at .254, so a batch holds at most %d devices",
			maxPerBatch, itestIPBase, maxSweepBatch)
	}

	var gateways, others []string
	for _, m := range models {
		t, ok := typeOf(m)
		if !ok {
			return nil, fmt.Errorf("unknown model %q", m)
		}
		// ugw and uxg are both gateway families; a site adopts one of
		// either, so they compete for the single per-batch gateway slot
		// rather than landing in the non-gateway pool.
		if t == "ugw" || t == "uxg" {
			gateways = append(gateways, m)
		} else {
			others = append(others, m)
		}
	}

	count := (len(models) + maxPerBatch - 1) / maxPerBatch
	if len(gateways) > count {
		count = len(gateways)
	}
	batches := make([]sweepBatch, count)
	for i := range batches {
		batches[i].Index = i
		if i < len(gateways) {
			batches[i].Models = []string{gateways[i]}
		}
	}
	// Free slots are count*maxPerBatch - len(gateways), which is at least
	// len(others) because count*maxPerBatch >= len(models); filling batches
	// in order therefore always places every non-gateway, and no batch is
	// left empty.
	next := 0
	for i := range batches {
		for len(batches[i].Models) < maxPerBatch && next < len(others) {
			batches[i].Models = append(batches[i].Models, others[next])
			next++
		}
	}
	return batches, nil
}

// --- unit tests (no container runtime needed) ---

func TestPlanSweepInvariants(t *testing.T) {
	types := map[string]string{
		"UGW3": "ugw", "UGW4": "ugw", "UXGENT": "uxg", "UXGPRO": "uxg",
		"USWED74": "usw", "USM8P": "usw", "USW24": "usw",
		"U7MP": "uap", "U7PRO": "uap", "UAPA6B0": "uap",
	}
	tests := []struct {
		name        string
		models      []string
		maxPerBatch int
		wantBatches int
	}{
		{"empty catalog", nil, 8, 0},
		{"single non-gateway", []string{"U7PRO"}, 8, 1},
		{"single gateway", []string{"UGW3"}, 8, 1},
		// Gateways alone still need one boot each.
		{"gateways only", []string{"UGW3", "UGW4", "UXGENT"}, 8, 3},
		{"non-gateways only", []string{"USM8P", "USW24", "U7MP", "U7PRO", "UAPA6B0"}, 2, 3},
		// Two gateways, eight models, four per batch: the size bound and
		// the gateway count agree at two boots.
		{"gateways fit the size bound", []string{"UGW3", "UXGENT", "USM8P", "USW24", "USWED74", "U7MP", "U7PRO", "UAPA6B0"}, 4, 2},
		// Four gateways in six models would fit one boot on size alone;
		// the one-gateway-per-site rule forces four.
		{"gateways force extra boots", []string{"UGW3", "UGW4", "UXGENT", "UXGPRO", "U7PRO", "USM8P"}, 6, 4},
		{"one model per batch", []string{"UGW3", "UXGENT", "U7PRO", "USM8P"}, 1, 4},
		{"batch bigger than the catalog", []string{"UGW3", "U7PRO", "USM8P"}, 154, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := planSweep(tt.models, fakeTypeOf(types), tt.maxPerBatch)
			if err != nil {
				t.Fatalf("planSweep: %v", err)
			}
			if len(got) != tt.wantBatches {
				t.Fatalf("got %d batches, want %d: %v", len(got), tt.wantBatches, got)
			}
			seen := map[string]int{}
			for i, b := range got {
				if b.Index != i {
					t.Errorf("batch %d has Index %d, want %d", i, b.Index, i)
				}
				if len(b.Models) == 0 {
					t.Errorf("batch %d is empty; a boot with no devices proves nothing", i)
				}
				if len(b.Models) > tt.maxPerBatch {
					t.Errorf("batch %d holds %d models, want <=%d: %v", i, len(b.Models), tt.maxPerBatch, b.Models)
				}
				gateways := 0
				for _, m := range b.Models {
					seen[m]++
					if types[m] == "ugw" || types[m] == "uxg" {
						gateways++
					}
				}
				if gateways > 1 {
					t.Errorf("batch %d has %d gateways, want <=1: %v", i, gateways, b.Models)
				}
			}
			for _, m := range tt.models {
				if seen[m] != 1 {
					t.Errorf("model %q appears %d times across the plan, want 1", m, seen[m])
				}
				delete(seen, m)
			}
			for m := range seen {
				t.Errorf("plan invented model %q", m)
			}
		})
	}
}

// TestPlanSweepDeterministic pins both halves of "same inputs, same output":
// the exact layout of one plan, and that repeated calls reproduce it. A
// sweep that reshuffles between runs makes "batch 3 failed" meaningless.
func TestPlanSweepDeterministic(t *testing.T) {
	types := map[string]string{
		"UGW3": "ugw", "UXGENT": "uxg", "USM8P": "usw", "USW24": "usw",
		"U7MP": "uap", "U7PRO": "uap",
	}
	models := []string{"UGW3", "USM8P", "UXGENT", "U7PRO", "USW24", "U7MP"}
	want := []sweepBatch{
		{Index: 0, Models: []string{"UGW3", "USM8P", "U7PRO"}},
		{Index: 1, Models: []string{"UXGENT", "USW24", "U7MP"}},
	}
	first, err := planSweep(models, fakeTypeOf(types), 3)
	if err != nil {
		t.Fatalf("planSweep: %v", err)
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("got %v, want %v", first, want)
	}
	for i := range 50 {
		got, err := planSweep(models, fakeTypeOf(types), 3)
		if err != nil {
			t.Fatalf("run %d: planSweep: %v", i, err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs: %v vs %v", i, got, first)
		}
	}
}

func TestPlanSweepErrors(t *testing.T) {
	types := map[string]string{"UGW3": "ugw", "U7PRO": "uap"}
	tests := []struct {
		name        string
		models      []string
		maxPerBatch int
		wantIn      string
	}{
		{"zero batch size", []string{"U7PRO"}, 0, "at least 1"},
		{"negative batch size", []string{"U7PRO"}, -3, "at least 1"},
		// Past 154 the harness cannot hand out addresses at all.
		{"batch past the /24", []string{"U7PRO"}, 155, ".254"},
		{"batch far past the /24", nil, 1000, ".254"},
		{"unknown model", []string{"U7PRO", "UGWHD4"}, 8, `unknown model "UGWHD4"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := planSweep(tt.models, fakeTypeOf(types), tt.maxPerBatch)
			if err == nil {
				t.Fatalf("planSweep returned %v, want an error", got)
			}
			if got != nil {
				t.Errorf("planSweep returned %v alongside the error, want nil", got)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error %q does not mention %q", err, tt.wantIn)
			}
		})
	}
}

// TestPlanSweepCatalog runs the planner over the real embedded registry —
// the input the sweep actually gets — so registry drift (a new gateway
// family, a model count crossing a batch boundary) shows up here.
func TestPlanSweepCatalog(t *testing.T) {
	models := emu.Models()
	if len(models) == 0 {
		t.Fatal("registry is empty")
	}
	typeOf := func(m string) (string, bool) {
		p, ok := emu.Profile(m)
		return p.Type, ok
	}
	isGateway := func(m string) bool {
		p, _ := emu.Profile(m)
		return p.Type == "ugw" || p.Type == "uxg"
	}

	const maxPerBatch = 24
	got, err := planSweep(models, typeOf, maxPerBatch)
	if err != nil {
		t.Fatalf("planSweep over the registry: %v", err)
	}

	totalGateways := 0
	for _, m := range models {
		if isGateway(m) {
			totalGateways++
		}
	}
	if totalGateways == 0 {
		t.Fatal("registry has no gateways; the one-per-batch rule is untested")
	}
	wantBatches := (len(models) + maxPerBatch - 1) / maxPerBatch
	if totalGateways > wantBatches {
		wantBatches = totalGateways
	}
	if len(got) != wantBatches {
		t.Errorf("got %d batches for %d models / %d gateways, want %d", len(got), len(models), totalGateways, wantBatches)
	}

	seen := map[string]int{}
	for i, b := range got {
		if b.Index != i {
			t.Errorf("batch %d has Index %d, want %d", i, b.Index, i)
		}
		if len(b.Models) == 0 || len(b.Models) > maxPerBatch {
			t.Errorf("batch %d holds %d models, want 1-%d", i, len(b.Models), maxPerBatch)
		}
		gateways := 0
		for _, m := range b.Models {
			seen[m]++
			if isGateway(m) {
				gateways++
			}
		}
		if gateways > 1 {
			t.Errorf("batch %d has %d gateways, want <=1: %v", i, gateways, b.Models)
		}
	}
	for _, m := range models {
		if seen[m] != 1 {
			t.Errorf("model %q appears %d times across the plan, want 1", m, seen[m])
		}
	}
	if len(seen) != len(models) {
		t.Errorf("plan covers %d distinct models, registry has %d", len(seen), len(models))
	}
}
