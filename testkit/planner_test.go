package testkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	emu "github.com/jamesbraid/unifi-emu"
)

func TestCatalogValidation(t *testing.T) {
	cases := []struct {
		name    string
		catalog Catalog
		wantErr string
	}{
		{
			name: "valid catalog",
			catalog: Catalog{
				Version: 1,
				Runtimes: []Runtime{
					{
						Name:      "fixture",
						Models:    []string{"U7PRO"},
						Image:     "example.invalid/fixture:local",
						Isolation: "device",
					},
				},
			},
			wantErr: "",
		},
		{
			name: "wrong version",
			catalog: Catalog{
				Version: 2,
				Runtimes: []Runtime{
					{
						Name:      "fixture",
						Models:    []string{"U7PRO"},
						Image:     "example.invalid/fixture:local",
						Isolation: "device",
					},
				},
			},
			wantErr: "unsupported catalog version",
		},
		{
			name: "empty runtime name",
			catalog: Catalog{
				Version: 1,
				Runtimes: []Runtime{
					{
						Name:      "",
						Models:    []string{"U7PRO"},
						Image:     "example.invalid/fixture:local",
						Isolation: "device",
					},
				},
			},
			wantErr: "runtime name cannot be empty",
		},
		{
			name: "duplicate runtime name",
			catalog: Catalog{
				Version: 1,
				Runtimes: []Runtime{
					{
						Name:      "fixture",
						Models:    []string{"U7PRO"},
						Image:     "example.invalid/fixture:local",
						Isolation: "device",
					},
					{
						Name:      "fixture",
						Models:    []string{"USM8P"},
						Image:     "example.invalid/fixture:local",
						Isolation: "device",
					},
				},
			},
			wantErr: "duplicate runtime name",
		},
		{
			name: "empty image",
			catalog: Catalog{
				Version: 1,
				Runtimes: []Runtime{
					{
						Name:      "fixture",
						Models:    []string{"U7PRO"},
						Image:     "",
						Isolation: "device",
					},
				},
			},
			wantErr: "image cannot be empty",
		},
		{
			name: "empty models",
			catalog: Catalog{
				Version: 1,
				Runtimes: []Runtime{
					{
						Name:      "fixture",
						Models:    []string{},
						Image:     "example.invalid/fixture:local",
						Isolation: "device",
					},
				},
			},
			wantErr: "models cannot be empty",
		},
		{
			name: "invalid isolation",
			catalog: Catalog{
				Version: 1,
				Runtimes: []Runtime{
					{
						Name:      "fixture",
						Models:    []string{"U7PRO"},
						Image:     "example.invalid/fixture:local",
						Isolation: "invalid",
					},
				},
			},
			wantErr: "invalid isolation",
		},
		{
			name: "duplicate model assignment",
			catalog: Catalog{
				Version: 1,
				Runtimes: []Runtime{
					{
						Name:      "fixture1",
						Models:    []string{"U7PRO"},
						Image:     "example.invalid/fixture:local",
						Isolation: "device",
					},
					{
						Name:      "fixture2",
						Models:    []string{"U7PRO"},
						Image:     "example.invalid/fixture:local",
						Isolation: "device",
					},
				},
			},
			wantErr: "multiple runtimes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.catalog.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestLoadCatalogStrictness(t *testing.T) {
	t.Run("unknown fields", func(t *testing.T) {
		content := `{
			"version": 1,
			"extra_field": 42,
			"runtimes": []
		}`
		p := filepath.Join(t.TempDir(), "catalog.json")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadCatalog(p)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("expected unknown field error, got: %v", err)
		}
	})

	t.Run("trailing json", func(t *testing.T) {
		content := `{
			"version": 1,
			"runtimes": []
		} {}`
		p := filepath.Join(t.TempDir(), "catalog.json")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadCatalog(p)
		if err == nil || !strings.Contains(err.Error(), "trailing JSON") {
			t.Fatalf("expected trailing JSON error, got: %v", err)
		}
	})
}

func TestPlanLaunch(t *testing.T) {
	cat := &Catalog{
		Version: 1,
		Runtimes: []Runtime{
			{
				Name:      "fixture-device",
				Models:    []string{"U7PRO"},
				Image:     "example.invalid/public-fixture:local",
				Isolation: "device",
			},
			{
				Name:      "fixture-fleet",
				Models:    []string{"USM8P"},
				Image:     "example.invalid/public-fleet:local",
				Isolation: "fleet",
			},
		},
	}

	specs := []emu.DeviceSpec{
		{MAC: "00:27:22:e0:00:01", Model: "UGW3"},
		{MAC: "00:27:22:e0:00:02", Model: "U7PRO"},
		{MAC: "00:27:22:e0:00:03", Model: "USM8P"},
		{MAC: "00:27:22:e0:00:04", Model: "UGW3"},
		{MAC: "00:27:22:e0:00:05", Model: "U7PRO"},
		{MAC: "00:27:22:e0:00:06", Model: "USM8P"},
	}

	plan, err := PlanLaunch(specs, cat)
	if err != nil {
		t.Fatalf("unexpected error planning launch: %v", err)
	}

	// We expect:
	// 1. Synthetic fleet (UGW3) - 1 container (batched) with 2 devices (macs: 00:01, 00:04)
	// 2. fixture-device (U7PRO) - 2 containers (device-isolated), 1 device each:
	//    - container 1 (mac: 00:02)
	//    - container 2 (mac: 00:05)
	// 3. fixture-fleet (USM8P) - 1 container (fleet-isolated) with 2 devices (macs: 00:03, 00:06)
	// Let's verify the container plan details and ordering.
	// Order of containers:
	// - UGW3 (synthetic) is first encountered -> synthetic plan created at index 0.
	// - U7PRO (device) is encountered -> device plan created at index 1.
	// - USM8P (fleet) is encountered -> fleet plan created at index 2.
	// - UGW3 (synthetic) -> added to index 0 plan.
	// - U7PRO (device) -> new device plan created at index 3.
	// - USM8P (fleet) -> added to index 2 plan.
	// So plan length should be 4:
	// [0]: synthetic (UGW3: 00:01, 00:04)
	// [1]: fixture-device (U7PRO: 00:02)
	// [2]: fixture-fleet (USM8P: 00:03, 00:06)
	// [3]: fixture-device (U7PRO: 00:05)
	if len(plan.Containers) != 4 {
		t.Fatalf("expected 4 planned containers, got: %d", len(plan.Containers))
	}

	// [0]
	c0 := plan.Containers[0]
	if c0.RuntimeName != "synthetic" || c0.Isolation != "synthetic" || len(c0.Specs) != 2 {
		t.Errorf("c0 mismatch: %+v", c0)
	}
	if c0.Specs[0].MAC != "00:27:22:e0:00:01" || c0.Specs[1].MAC != "00:27:22:e0:00:04" {
		t.Errorf("c0 specs order mismatch: %+v", c0.Specs)
	}
	if c0.LogName != "emulator-synthetic.log" {
		t.Errorf("c0 log name mismatch: %s", c0.LogName)
	}

	// [1]
	c1 := plan.Containers[1]
	if c1.RuntimeName != "fixture-device" || c1.Isolation != "device" || len(c1.Specs) != 1 {
		t.Errorf("c1 mismatch: %+v", c1)
	}
	if c1.Specs[0].MAC != "00:27:22:e0:00:02" {
		t.Errorf("c1 specs mismatch: %+v", c1.Specs)
	}
	if c1.LogName != "emulator-fixture-device-002722e00002.log" {
		t.Errorf("c1 log name mismatch: %s", c1.LogName)
	}

	// [2]
	c2 := plan.Containers[2]
	if c2.RuntimeName != "fixture-fleet" || c2.Isolation != "fleet" || len(c2.Specs) != 2 {
		t.Errorf("c2 mismatch: %+v", c2)
	}
	if c2.Specs[0].MAC != "00:27:22:e0:00:03" || c2.Specs[1].MAC != "00:27:22:e0:00:06" {
		t.Errorf("c2 specs mismatch: %+v", c2.Specs)
	}
	if c2.LogName != "emulator-fixture-fleet.log" {
		t.Errorf("c2 log name mismatch: %s", c2.LogName)
	}

	// [3]
	c3 := plan.Containers[3]
	if c3.RuntimeName != "fixture-device" || c3.Isolation != "device" || len(c3.Specs) != 1 {
		t.Errorf("c3 mismatch: %+v", c3)
	}
	if c3.Specs[0].MAC != "00:27:22:e0:00:05" {
		t.Errorf("c3 specs mismatch: %+v", c3.Specs)
	}
	if c3.LogName != "emulator-fixture-device-002722e00005.log" {
		t.Errorf("c3 log name mismatch: %s", c3.LogName)
	}
}

func TestPlanLaunchSingleSynthetic(t *testing.T) {
	// Sole synthetic container log name should be "emulator.log"
	specs := []emu.DeviceSpec{
		{MAC: "00:27:22:e0:00:01", Model: "UGW3"},
	}
	plan, err := PlanLaunch(specs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(plan.Containers))
	}
	if plan.Containers[0].LogName != "emulator.log" {
		t.Fatalf("expected log name emulator.log, got %s", plan.Containers[0].LogName)
	}
}

func TestPlanLaunchEmpty(t *testing.T) {
	_, err := PlanLaunch(nil, nil)
	if err == nil {
		t.Fatal("expected planning empty specs to fail")
	}
}
