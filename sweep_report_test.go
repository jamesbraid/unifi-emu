package emu_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// sweepOutcome is what the sweep measured for one model.
type sweepOutcome struct {
	Model      string `json:"model"`
	Type       string `json:"type"`
	MAC        string `json:"mac"`
	Registered bool   `json:"registered"`
	// Adopted is written even when false: in an adoption sweep, a model that
	// registered and then failed to adopt is the interesting result, and
	// omitting the field would record it as indistinguishable from a model
	// nobody measured. The report-level count keeps omitempty, where absence
	// genuinely means "this tier never checked adoption".
	Adopted bool   `json:"adopted"`
	Batch   int    `json:"batch"`
	Note    string `json:"note,omitempty"`
}

// sweepReport is one whole sweep run.
type sweepReport struct {
	Tier       string         `json:"tier"`
	Total      int            `json:"total"`
	Registered int            `json:"registered"`
	Adopted    int            `json:"adopted,omitempty"`
	Failures   []sweepOutcome `json:"failures"`
	Outcomes   []sweepOutcome `json:"outcomes"`
}

// newSweepReport tallies outcomes into a report.
// tier is "registration" or "adoption".
//
// The outcomes are copied before sorting: the caller boots models in batches
// and keeps that order for its own bookkeeping, so the report must not
// reshuffle the slice underneath it.
func newSweepReport(tier string, outcomes []sweepOutcome) sweepReport {
	sorted := make([]sweepOutcome, len(outcomes))
	copy(sorted, outcomes)
	slices.SortStableFunc(sorted, func(a, b sweepOutcome) int {
		return strings.Compare(a.Model, b.Model)
	})

	report := sweepReport{
		Tier:     tier,
		Total:    len(sorted),
		Failures: make([]sweepOutcome, 0, len(sorted)),
		Outcomes: sorted,
	}
	for _, outcome := range sorted {
		if outcome.Registered {
			report.Registered++
		}
		if outcome.Adopted {
			report.Adopted++
		}
		// The adoption tier's bar is deliberately stricter. Registration only
		// proves the controller listed the device from its inform; adoption
		// proves it survived the set-adopt handshake and reached CONNECTED.
		// Models that register and then refuse to adopt are exactly what the
		// second tier exists to find, so registration alone cannot clear it.
		cleared := outcome.Registered
		if tier == "adoption" {
			cleared = cleared && outcome.Adopted
		}
		if !cleared {
			report.Failures = append(report.Failures, outcome)
		}
	}
	return report
}

// summary is a one-line human-readable result, e.g.
// "registration sweep: 185/186 registered, 1 failed: UGWHD4"
//
// It names the failures inline because a bare count sends whoever is reading
// CI output digging through the JSON for the one model that matters.
func (r sweepReport) summary() string {
	verb := "registered"
	if r.Tier == "adoption" {
		verb = "adopted"
	}
	line := fmt.Sprintf("%s sweep: %d/%d %s", r.Tier, r.Total-len(r.Failures), r.Total, verb)

	failed := r.failedModels()
	if len(failed) == 0 {
		return line + ", no failures"
	}
	// A broken controller fails the whole catalog at once. Naming all 186
	// models would bury the line it is meant to summarize, so cap the list
	// and let the report file carry the rest.
	const maxNamed = 10
	if len(failed) > maxNamed {
		return fmt.Sprintf("%s, %d failed: %s and %d more",
			line, len(failed), strings.Join(failed[:maxNamed], ", "), len(failed)-maxNamed)
	}
	return fmt.Sprintf("%s, %d failed: %s", line, len(failed), strings.Join(failed, ", "))
}

// failedModels returns the models that did not clear the tier's bar, sorted.
// Failures is built from the already-sorted outcomes, so the order is stable
// across runs and a committed report diffs cleanly.
func (r sweepReport) failedModels() []string {
	models := make([]string, 0, len(r.Failures))
	for _, failure := range r.Failures {
		models = append(models, failure.Model)
	}
	return models
}

// --- unit tests (no container runtime needed) ---

// TestNewSweepReportTierBar pins the difference between the tiers: the
// registration tier only asks whether the controller listed the model, the
// adoption tier also demands it adopted. Registered-but-not-adopted is the
// case that must split the two, and it is the whole reason for tier two.
func TestNewSweepReportTierBar(t *testing.T) {
	tests := []struct {
		name       string
		tier       string
		outcome    sweepOutcome
		wantFailed bool
	}{
		{"registration/registered", "registration", sweepOutcome{Model: "U7PRO", Registered: true}, false},
		{"registration/missing", "registration", sweepOutcome{Model: "U7PRO"}, true},
		{
			"registration ignores adoption",
			"registration",
			sweepOutcome{Model: "U7PRO", Registered: true, Adopted: false},
			false,
		},
		{
			"adoption/adopted",
			"adoption",
			sweepOutcome{Model: "U7PRO", Registered: true, Adopted: true},
			false,
		},
		{
			"adoption/registered only",
			"adoption",
			sweepOutcome{Model: "U7PRO", Registered: true, Adopted: false},
			true,
		},
		{"adoption/missing", "adoption", sweepOutcome{Model: "U7PRO"}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := newSweepReport(test.tier, []sweepOutcome{test.outcome})
			gotFailed := len(report.Failures) == 1
			if gotFailed != test.wantFailed {
				t.Fatalf("%s tier, outcome %+v: failed = %v, want %v",
					test.tier, test.outcome, gotFailed, test.wantFailed)
			}
			if report.Total != 1 {
				t.Fatalf("Total = %d, want 1", report.Total)
			}
		})
	}
}

// TestNewSweepReportSortsAndCounts checks the two properties a committed
// report file depends on: model order (stable diffs) and honest totals.
func TestNewSweepReportSortsAndCounts(t *testing.T) {
	outcomes := []sweepOutcome{
		{Model: "USM8P", Registered: true, Adopted: true, Batch: 2},
		{Model: "UGW3", Registered: true, Adopted: false, Batch: 1, Note: "adopt timed out"},
		{Model: "U7PRO", Registered: true, Adopted: true, Batch: 1},
		{Model: "UAPA6B0", Registered: false, Batch: 3, Note: "never listed"},
	}
	input := slices.Clone(outcomes)

	report := newSweepReport("adoption", input)

	wantOrder := []string{"U7PRO", "UAPA6B0", "UGW3", "USM8P"}
	var gotOrder []string
	for _, outcome := range report.Outcomes {
		gotOrder = append(gotOrder, outcome.Model)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("outcome order = %v, want %v", gotOrder, wantOrder)
	}
	if got, want := report.failedModels(), []string{"UAPA6B0", "UGW3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("failedModels() = %v, want %v", got, want)
	}
	if report.Total != 4 || report.Registered != 3 || report.Adopted != 2 {
		t.Fatalf("counts = %d total / %d registered / %d adopted, want 4/3/2",
			report.Total, report.Registered, report.Adopted)
	}
	// The caller keeps its own batch ordering; sorting must not reach back
	// into the slice it was handed.
	if !reflect.DeepEqual(input, outcomes) {
		t.Fatalf("newSweepReport reordered the caller's slice: %v", input)
	}
}

// TestNewSweepReportEmpty covers the run that measured nothing (bad filter,
// controller never came up): it must produce a usable report, not a panic.
func TestNewSweepReportEmpty(t *testing.T) {
	report := newSweepReport("registration", nil)

	if report.Total != 0 || report.Registered != 0 || report.Adopted != 0 {
		t.Fatalf("counts = %+v, want all zero", report)
	}
	if report.Failures == nil || len(report.Failures) != 0 {
		t.Fatalf("Failures = %#v, want empty non-nil", report.Failures)
	}
	if report.Outcomes == nil || len(report.Outcomes) != 0 {
		t.Fatalf("Outcomes = %#v, want empty non-nil", report.Outcomes)
	}
	if got, want := report.summary(), "registration sweep: 0/0 registered, no failures"; got != want {
		t.Fatalf("summary() = %q, want %q", got, want)
	}
}

func TestSweepReportSummary(t *testing.T) {
	// twelve failures: past the cap, so the line must truncate.
	catastrophe := make([]sweepOutcome, 0, 12)
	for i := range 12 {
		catastrophe = append(catastrophe, sweepOutcome{Model: fmt.Sprintf("M%02d", i)})
	}

	tests := []struct {
		name     string
		tier     string
		outcomes []sweepOutcome
		want     string
	}{
		{
			name: "one failure named",
			tier: "registration",
			outcomes: []sweepOutcome{
				{Model: "UGWHD4"},
				{Model: "U7PRO", Registered: true},
				{Model: "USM8P", Registered: true},
			},
			want: "registration sweep: 2/3 registered, 1 failed: UGWHD4",
		},
		{
			name: "no failures",
			tier: "registration",
			outcomes: []sweepOutcome{
				{Model: "U7PRO", Registered: true},
				{Model: "USM8P", Registered: true},
			},
			want: "registration sweep: 2/2 registered, no failures",
		},
		{
			name: "adoption wording and stricter bar",
			tier: "adoption",
			outcomes: []sweepOutcome{
				{Model: "U7PRO", Registered: true, Adopted: true},
				{Model: "UGW3", Registered: true},
			},
			want: "adoption sweep: 1/2 adopted, 1 failed: UGW3",
		},
		{
			name:     "truncated at ten",
			tier:     "registration",
			outcomes: catastrophe,
			want: "registration sweep: 0/12 registered, 12 failed: " +
				"M00, M01, M02, M03, M04, M05, M06, M07, M08, M09 and 2 more",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := newSweepReport(test.tier, test.outcomes).summary()
			if got != test.want {
				t.Fatalf("summary() = %q, want %q", got, test.want)
			}
			if strings.Contains(got, "\n") {
				t.Fatalf("summary() must stay one line: %q", got)
			}
		})
	}
}

func TestSweepReportFailedModels(t *testing.T) {
	tests := []struct {
		name     string
		tier     string
		outcomes []sweepOutcome
		want     []string
	}{
		{
			name: "none failed",
			tier: "adoption",
			outcomes: []sweepOutcome{
				{Model: "U7PRO", Registered: true, Adopted: true},
				{Model: "UGW3", Registered: true, Adopted: true},
			},
			want: []string{},
		},
		{
			name: "all failed, sorted",
			tier: "adoption",
			outcomes: []sweepOutcome{
				{Model: "USM8P", Registered: true},
				{Model: "U7PRO"},
				{Model: "UGW3", Registered: true},
			},
			want: []string{"U7PRO", "UGW3", "USM8P"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := newSweepReport(test.tier, test.outcomes).failedModels()
			if got == nil {
				t.Fatal("failedModels() = nil, want empty non-nil slice")
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("failedModels() = %#v, want %#v", got, test.want)
			}
		})
	}
}

// TestSweepReportJSONRoundTrip guards the on-disk shape. The report is
// committed and diffed across runs, so the field names are a data contract:
// renaming one silently breaks every comparison against an older sweep.
func TestSweepReportJSONRoundTrip(t *testing.T) {
	report := newSweepReport("adoption", []sweepOutcome{
		{
			Model: "UGW3", Type: "ugw", MAC: "00:27:22:e0:00:01",
			Registered: true, Adopted: false, Batch: 1, Note: "adopt timed out",
		},
		{
			Model: "U7PRO", Type: "uap", MAC: "00:27:22:e0:00:02",
			Registered: true, Adopted: true, Batch: 2,
		},
	})

	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"tier":"adoption"`, `"total":2`, `"registered":2`, `"adopted":1`,
		`"failures":[`, `"outcomes":[`,
		`"model":"UGW3"`, `"type":"ugw"`, `"mac":"00:27:22:e0:00:01"`,
		`"batch":1`, `"note":"adopt timed out"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("report JSON missing %s:\n%s", want, body)
		}
	}

	var got sweepReport
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, report) {
		t.Fatalf("round trip = %#v, want %#v", got, report)
	}
}

// TestSweepReportJSONOmitsUnmeasuredAdoption keeps a registration-tier report
// from claiming an adoption count it never measured — a zero there would read
// as "186 models failed to adopt" rather than "adoption was not tested".
func TestSweepReportJSONOmitsUnmeasuredAdoption(t *testing.T) {
	report := newSweepReport("registration", []sweepOutcome{
		{Model: "U7PRO", Type: "uap", Registered: true, Batch: 1},
		{Model: "UGW3", Type: "ugw", Registered: true, Batch: 1},
	})

	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Only the report-level count may be absent. Per-model outcomes always
	// carry "adopted", because there a false is a measurement, not a gap.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := top["adopted"]; ok {
		t.Fatalf("registration report emits an adoption count:\n%s", body)
	}
	var outcomes []map[string]json.RawMessage
	if err := json.Unmarshal(top["outcomes"], &outcomes); err != nil {
		t.Fatalf("unmarshal outcomes: %v", err)
	}
	for _, outcome := range outcomes {
		if _, ok := outcome["adopted"]; !ok {
			t.Errorf("outcome %v drops the adopted field; a false there is a result", outcome)
		}
	}
}
