package herder

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
)

func TestCheckReaperRejectsADisabledReaper(t *testing.T) {
	cfg := testcontainers.TestcontainersConfig{}
	cfg.Config.RyukDisabled = true
	err := checkReaper(cfg)
	assertFailure(t, err, CodeReaperDisabled, PhaseValidate)
}

func TestCheckReaperAcceptsAnEnabledReaper(t *testing.T) {
	if err := checkReaper(testcontainers.TestcontainersConfig{}); err != nil {
		t.Fatalf("checkReaper with the reaper enabled = %v, want nil", err)
	}
}

// The herder reads effective Testcontainers behaviour, not just its own
// process environment: an inherited TESTCONTAINERS_RYUK_DISABLED must fail
// the run before any container is created, whatever set it. The check runs in
// a subprocess because testcontainers caches its configuration per process.
func TestEffectiveReaperConfigHonoursTheEnvironment(t *testing.T) {
	out, err := runReaperProbe(t, map[string]string{
		"HOME":                         t.TempDir(),
		"TESTCONTAINERS_RYUK_DISABLED": "true",
	})
	if err == nil {
		t.Fatalf("probe succeeded with the reaper disabled: %s", out)
	}
	if !strings.Contains(out, string(CodeReaperDisabled)) {
		t.Fatalf("probe output = %q, want %s", out, CodeReaperDisabled)
	}
}

// The properties file is the other documented input, and the design calls it
// out by name: a runner can disable the reaper without any environment
// variable at all.
func TestEffectiveReaperConfigHonoursThePropertiesFile(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(home+"/.testcontainers.properties", []byte("ryuk.disabled=true\n"), 0o600); err != nil {
		t.Fatalf("write properties: %v", err)
	}
	out, err := runReaperProbe(t, map[string]string{"HOME": home})
	if err == nil {
		t.Fatalf("probe succeeded with the reaper disabled by properties: %s", out)
	}
	if !strings.Contains(out, string(CodeReaperDisabled)) {
		t.Fatalf("probe output = %q, want %s", out, CodeReaperDisabled)
	}
}

func TestEffectiveReaperConfigAcceptsADefaultRunner(t *testing.T) {
	out, err := runReaperProbe(t, map[string]string{"HOME": t.TempDir()})
	if err != nil {
		t.Fatalf("probe failed on a default runner: %v (%s)", err, out)
	}
}

// TestReaperProbeHelper is the subprocess body: it reports the effective
// reaper decision and is skipped in a normal run.
func TestReaperProbeHelper(t *testing.T) {
	if os.Getenv("HERDER_REAPER_PROBE") != "1" {
		t.Skip("helper process")
	}
	if err := CheckEffectiveReaper(); err != nil {
		t.Fatalf("%v", err)
	}
}

func runReaperProbe(t *testing.T, env map[string]string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestReaperProbeHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "HERDER_REAPER_PROBE=1")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
