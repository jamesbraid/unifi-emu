package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInformURLDefault(t *testing.T) {
	// UNIFI_EMU_INFORM_URL is the device-container variable and beats the
	// older SIM_CONTROLLER; the -inform flag beats both by being a flag.
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"neither set": {nil, "http://localhost:8080/inform"},
		"sim only": {
			map[string]string{"SIM_CONTROLLER": "http://sim:8080/inform"},
			"http://sim:8080/inform",
		},
		"unifi_emu only": {
			map[string]string{"UNIFI_EMU_INFORM_URL": "http://new:8080/inform"},
			"http://new:8080/inform",
		},
		"unifi_emu wins": {
			map[string]string{
				"UNIFI_EMU_INFORM_URL": "http://new:8080/inform",
				"SIM_CONTROLLER":       "http://sim:8080/inform",
			},
			"http://new:8080/inform",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := informURLDefault(func(k string) string { return tc.env[k] })
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadyFilePath(t *testing.T) {
	if got := readyFilePath(func(string) string { return "" }); got != defaultReadyFile {
		t.Errorf("got %q, want %q", got, defaultReadyFile)
	}
	t.Setenv("UNIFI_EMU_READY_FILE", "/tmp/ready")
	if got := readyFilePath(os.Getenv); got != "/tmp/ready" {
		t.Errorf("got %q, want /tmp/ready", got)
	}
}

func TestReadyProbe(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ready")
	t.Setenv("UNIFI_EMU_READY_FILE", marker)

	// Nothing has started yet: the probe must fail so the container is
	// reported unhealthy rather than healthy-by-default.
	if got := readyProbe(readyFilePath(os.Getenv)); got != 1 {
		t.Errorf("probe before the marker exists = %d, want 1", got)
	}
	if err := writeReady(readyFilePath(os.Getenv)); err != nil {
		t.Fatalf("writeReady: %v", err)
	}
	if got := readyProbe(readyFilePath(os.Getenv)); got != 0 {
		t.Errorf("probe after the marker exists = %d, want 0", got)
	}
}

func TestWriteReadyFails(t *testing.T) {
	// An unwritable marker path is fatal to the run: a container that can
	// never turn healthy must not pretend to be running.
	marker := filepath.Join(t.TempDir(), "nodir", "ready")
	if err := writeReady(marker); err == nil {
		t.Fatalf("writeReady(%s) = nil, want an error", marker)
	}
}
