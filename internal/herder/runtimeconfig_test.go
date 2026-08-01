package herder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtimes.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func loadConfigBody(t *testing.T, body string) (RuntimeConfig, error) {
	t.Helper()
	return LoadRuntimeConfig(writeConfig(t, body))
}

func TestLoadRuntimeConfigAcceptsTheDocumentedShape(t *testing.T) {
	got, err := loadConfigBody(t, `{
	  "version": 1,
	  "models": {
	    "UXGENT": {
	      "image": "forge.example/emu/uxgent@`+testDigest+`",
	      "cap_add": ["NET_ADMIN", "NET_RAW"]
	    }
	  }
	}`)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	entry, ok := got.Models["UXGENT"]
	if !ok {
		t.Fatalf("models = %#v, want a UXGENT entry", got.Models)
	}
	if entry.Image != "forge.example/emu/uxgent@"+testDigest {
		t.Fatalf("image = %q", entry.Image)
	}
	if strings.Join(entry.CapAdd, ",") != "NET_ADMIN,NET_RAW" {
		t.Fatalf("cap_add = %#v", entry.CapAdd)
	}
}

func TestLoadRuntimeConfigAcceptsAnEmptyModelMap(t *testing.T) {
	got, err := loadConfigBody(t, `{"version":1,"models":{}}`)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	if len(got.Models) != 0 {
		t.Fatalf("models = %#v, want empty", got.Models)
	}
}

func TestLoadRuntimeConfigRejectsUnknownFields(t *testing.T) {
	for _, body := range []string{
		`{"version":1,"models":{},"registry":"forge.example"}`,
		`{"version":1,"models":{"M":{"image":"r/i@` + testDigest + `","privileged":true}}}`,
	} {
		_, err := loadConfigBody(t, body)
		assertFailure(t, err, CodeInvalidRuntimeConfig, PhaseValidate)
	}
}

func TestLoadRuntimeConfigRejectsTrailingDocument(t *testing.T) {
	_, err := loadConfigBody(t, `{"version":1,"models":{}}`+"\n"+`{"version":1,"models":{}}`)
	assertFailure(t, err, CodeInvalidRuntimeConfig, PhaseValidate)
}

func TestLoadRuntimeConfigRejectsUnsupportedVersion(t *testing.T) {
	_, err := loadConfigBody(t, `{"version":2,"models":{}}`)
	assertFailure(t, err, CodeInvalidRuntimeConfig, PhaseValidate)
}

// The runtime file is operator-owned and uses standard Go JSON member
// handling: a duplicate model key is not detected and the last value wins.
// This is documented behaviour, not an oversight, so it is pinned by a test
// rather than defended by a token-level duplicate-key scanner.
func TestDuplicateModelKeyTakesTheLastValue(t *testing.T) {
	got, err := loadConfigBody(t, `{"version":1,"models":{
	  "UXGENT": {"image": "first.example/a@`+testDigest+`"},
	  "UXGENT": {"image": "second.example/b@`+testDigest+`"}
	}}`)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("models = %#v, want one entry", got.Models)
	}
	if got.Models["UXGENT"].Image != "second.example/b@"+testDigest {
		t.Fatalf("image = %q, want the last value to win", got.Models["UXGENT"].Image)
	}
}

func TestModelKeysAreNotNormalized(t *testing.T) {
	got, err := loadConfigBody(t, `{"version":1,"models":{"uxgEnt":{"image":"r/i@`+testDigest+`"}}}`)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	if _, ok := got.Models["uxgEnt"]; !ok {
		t.Fatalf("models = %#v, want the key preserved verbatim", got.Models)
	}
}

func TestImageMustBeDigestPinned(t *testing.T) {
	bad := []string{
		"forge.example/emu/uxgent:1.2.3",
		"forge.example/emu/uxgent",
		"forge.example/emu/uxgent@sha256:short",
		"forge.example/emu/uxgent@sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef",
		"forge.example/emu/uxgent@sha512:" + strings.Repeat("a", 64),
		"@" + testDigest,
		"",
	}
	for _, image := range bad {
		_, err := loadConfigBody(t, `{"version":1,"models":{"M":{"image":"`+image+`"}}}`)
		if err == nil {
			t.Errorf("image %q accepted, want a digest-pin rejection", image)
			continue
		}
		assertFailure(t, err, CodeInvalidRuntimeConfig, PhaseValidate)
	}
}

func TestCapAddAcceptsOnlyTheCompiledAllowlist(t *testing.T) {
	for _, cap := range []string{"CHOWN", "NET_ADMIN", "SYS_PTRACE"} {
		if _, err := loadConfigBody(t,
			`{"version":1,"models":{"M":{"image":"r/i@`+testDigest+`","cap_add":["`+cap+`"]}}}`); err != nil {
			t.Errorf("capability %s rejected: %v", cap, err)
		}
	}
	for _, cap := range []string{"ALL", "CAP_NET_ADMIN", "net_admin", "SYS_MODULE", "AUDIT_WRITE", ""} {
		_, err := loadConfigBody(t,
			`{"version":1,"models":{"M":{"image":"r/i@`+testDigest+`","cap_add":["`+cap+`"]}}}`)
		if err == nil {
			t.Errorf("capability %q accepted, want a rejection", cap)
			continue
		}
		assertFailure(t, err, CodeInvalidRuntimeConfig, PhaseValidate)
	}
}

func TestCapAddRejectsDuplicates(t *testing.T) {
	_, err := loadConfigBody(t,
		`{"version":1,"models":{"M":{"image":"r/i@`+testDigest+`","cap_add":["NET_RAW","NET_RAW"]}}}`)
	assertFailure(t, err, CodeInvalidRuntimeConfig, PhaseValidate)
}

func TestLoadRuntimeConfigRejectsGroupOrWorldWritableFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o620, 0o606, 0o666} {
		path := writeConfig(t, `{"version":1,"models":{}}`)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		_, err := LoadRuntimeConfig(path)
		if err == nil {
			t.Errorf("mode %v accepted, want a rejection", mode)
			continue
		}
		assertFailure(t, err, CodeInvalidRuntimeConfig, PhaseValidate)
	}
}

func TestLoadRuntimeConfigRejectsNonRegularFile(t *testing.T) {
	_, err := LoadRuntimeConfig(t.TempDir())
	assertFailure(t, err, CodeInvalidRuntimeConfig, PhaseValidate)
}

func TestResolveRuntimeConfigPrefersTheFlag(t *testing.T) {
	flagPath := writeConfig(t, `{"version":1,"models":{"FLAG":{"image":"r/i@`+testDigest+`"}}}`)
	envPath := writeConfig(t, `{"version":1,"models":{"ENV":{"image":"r/i@`+testDigest+`"}}}`)
	got, err := ResolveRuntimeConfig(flagPath, envLookup{"UNIFI_EMU_RUNTIME_CONFIG": envPath}.get)
	if err != nil {
		t.Fatalf("ResolveRuntimeConfig: %v", err)
	}
	if _, ok := got.Models["FLAG"]; !ok {
		t.Fatalf("models = %#v, want the flag file", got.Models)
	}
}

func TestResolveRuntimeConfigFallsBackToTheEnvironment(t *testing.T) {
	envPath := writeConfig(t, `{"version":1,"models":{"ENV":{"image":"r/i@`+testDigest+`"}}}`)
	got, err := ResolveRuntimeConfig("", envLookup{"UNIFI_EMU_RUNTIME_CONFIG": envPath}.get)
	if err != nil {
		t.Fatalf("ResolveRuntimeConfig: %v", err)
	}
	if _, ok := got.Models["ENV"]; !ok {
		t.Fatalf("models = %#v, want the environment file", got.Models)
	}
}

func TestResolveRuntimeConfigUsesTheSystemFileWhenPresent(t *testing.T) {
	systemPath := writeConfig(t, `{"version":1,"models":{"SYSTEM":{"image":"r/i@`+testDigest+`"}}}`)
	restore := swapDefaultRuntimeConfigPath(systemPath)
	defer restore()
	got, err := ResolveRuntimeConfig("", envLookup{}.get)
	if err != nil {
		t.Fatalf("ResolveRuntimeConfig: %v", err)
	}
	if _, ok := got.Models["SYSTEM"]; !ok {
		t.Fatalf("models = %#v, want the system file", got.Models)
	}
}

// An absent default file means all-synthetic operation, which is the whole
// point: a public runner has no mapping installed and still runs the request.
func TestResolveRuntimeConfigWithNoFileIsAllSynthetic(t *testing.T) {
	restore := swapDefaultRuntimeConfigPath(filepath.Join(t.TempDir(), "absent.json"))
	defer restore()
	got, err := ResolveRuntimeConfig("", envLookup{}.get)
	if err != nil {
		t.Fatalf("ResolveRuntimeConfig: %v", err)
	}
	if len(got.Models) != 0 {
		t.Fatalf("models = %#v, want no mappings", got.Models)
	}
}

// An explicitly selected file that does not exist is an error: silently
// running all-synthetic would hide an operator's broken installation.
func TestResolveRuntimeConfigRejectsAMissingExplicitFile(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent.json")
	_, err := ResolveRuntimeConfig(absent, envLookup{}.get)
	assertFailure(t, err, CodeInvalidRuntimeConfig, PhaseValidate)

	_, err = ResolveRuntimeConfig("", envLookup{"UNIFI_EMU_RUNTIME_CONFIG": absent}.get)
	assertFailure(t, err, CodeInvalidRuntimeConfig, PhaseValidate)
}

type envLookup map[string]string

func (e envLookup) get(key string) string { return e[key] }
