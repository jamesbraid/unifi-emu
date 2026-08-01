package herder

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
)

// runtimeConfigVersion is the only runner-local configuration version this
// herder accepts.
const runtimeConfigVersion = 1

// runtimeConfigEnv names the environment variable that selects the file when
// no --runtime-config flag is given.
const runtimeConfigEnv = "UNIFI_EMU_RUNTIME_CONFIG"

// defaultRuntimeConfigPath is the system-wide runner-local mapping. It is a
// variable so tests can point the fallback somewhere harmless.
var defaultRuntimeConfigPath = "/etc/unifi-emu-herder/runtimes.json"

// swapDefaultRuntimeConfigPath repoints the system default and returns the
// restore func. Test-only, but it lives here beside the value it mutates.
func swapDefaultRuntimeConfigPath(path string) func() {
	previous := defaultRuntimeConfigPath
	defaultRuntimeConfigPath = path
	return func() { defaultRuntimeConfigPath = previous }
}

// capabilityAllowlist is the compiled capability vocabulary. Adding an entry
// is a public code review, which is the point: the runner-local file decides
// which models get an opaque runtime, but never what those runtimes may do.
var capabilityAllowlist = map[string]bool{
	"CHOWN": true, "DAC_OVERRIDE": true, "DAC_READ_SEARCH": true, "FOWNER": true,
	"KILL": true, "MKNOD": true, "NET_ADMIN": true, "NET_BIND_SERVICE": true,
	"NET_RAW": true, "SETGID": true, "SETUID": true, "SYS_ADMIN": true,
	"SYS_CHROOT": true, "SYS_PTRACE": true, "SYS_RESOURCE": true,
}

// RuntimeConfig is the runner-local opaque-runtime mapping. It has no
// defaults beyond an empty model map, and cannot express commands, mounts,
// environment, namespaces or privilege: an image must carry its complete root
// filesystem and ENTRYPOINT.
type RuntimeConfig struct {
	Version int                     `json:"version"`
	Models  map[string]ModelRuntime `json:"models"`
}

// ModelRuntime routes one model to a digest-pinned image and the smallest
// capability set Docker must add on top of a full drop.
type ModelRuntime struct {
	Image  string   `json:"image"`
	CapAdd []string `json:"cap_add"`
}

// Mapping returns the runtime mapped to model, if any. Model keys are matched
// verbatim: no normalization or case folding is applied, because the
// controller and the public registry both use case-sensitive model codes.
func (c RuntimeConfig) Mapping(model string) (ModelRuntime, bool) {
	rt, ok := c.Models[model]
	return rt, ok
}

// ResolveRuntimeConfig loads the runner-local mapping using the documented
// precedence: the flag, then the environment, then the system file when it
// exists, then no mappings at all. An explicitly selected file that is
// missing is an error -- silently running all-synthetic would hide a broken
// runner installation -- while an absent system file simply means this runner
// maps nothing.
func ResolveRuntimeConfig(flagPath string, getenv func(string) string) (RuntimeConfig, error) {
	if flagPath != "" {
		return LoadRuntimeConfig(flagPath)
	}
	if envPath := getenv(runtimeConfigEnv); envPath != "" {
		return LoadRuntimeConfig(envPath)
	}
	if _, err := os.Stat(defaultRuntimeConfigPath); err == nil {
		return LoadRuntimeConfig(defaultRuntimeConfigPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return RuntimeConfig{}, wrapf(err, CodeInvalidRuntimeConfig, PhaseValidate,
			"stat %s: %v", defaultRuntimeConfigPath, err)
	}
	return RuntimeConfig{Models: map[string]ModelRuntime{}}, nil
}

// LoadRuntimeConfig reads and validates one runtime file.
func LoadRuntimeConfig(path string) (RuntimeConfig, error) {
	info, err := os.Stat(path)
	if err != nil {
		return RuntimeConfig{}, wrapf(err, CodeInvalidRuntimeConfig, PhaseValidate,
			"runtime configuration %s: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		return RuntimeConfig{}, failf(CodeInvalidRuntimeConfig, PhaseValidate,
			"runtime configuration %s is not a regular file", path)
	}
	// A file another account can rewrite decides which image runs as a
	// device, so a writable-by-anyone-else mapping is refused outright.
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return RuntimeConfig{}, failf(CodeInvalidRuntimeConfig, PhaseValidate,
			"runtime configuration %s is group- or world-writable (mode %04o)", path, mode)
	}
	if err := checkOwner(path, info); err != nil {
		return RuntimeConfig{}, err
	}
	body, err := os.Open(path)
	if err != nil {
		return RuntimeConfig{}, wrapf(err, CodeInvalidRuntimeConfig, PhaseValidate,
			"open runtime configuration %s: %v", path, err)
	}
	defer func() { _ = body.Close() }()
	return decodeRuntimeConfig(body)
}

func decodeRuntimeConfig(r io.Reader) (RuntimeConfig, error) {
	var cfg RuntimeConfig
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return RuntimeConfig{}, wrapf(err, CodeInvalidRuntimeConfig, PhaseValidate,
			"decode runtime configuration: %v", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return RuntimeConfig{}, failf(CodeInvalidRuntimeConfig, PhaseValidate,
				"the runtime configuration carries more than one JSON document")
		}
		return RuntimeConfig{}, wrapf(err, CodeInvalidRuntimeConfig, PhaseValidate,
			"trailing input after the runtime configuration: %v", err)
	}
	if cfg.Version != runtimeConfigVersion {
		return RuntimeConfig{}, failf(CodeInvalidRuntimeConfig, PhaseValidate,
			"runtime configuration version %d is not supported (want %d)",
			cfg.Version, runtimeConfigVersion)
	}
	if cfg.Models == nil {
		cfg.Models = map[string]ModelRuntime{}
	}
	for model, rt := range cfg.Models {
		if err := checkImageDigest(model, rt.Image); err != nil {
			return RuntimeConfig{}, err
		}
		if err := checkCapabilities(model, rt.CapAdd); err != nil {
			return RuntimeConfig{}, err
		}
	}
	return cfg, nil
}

// checkImageDigest requires a repository@sha256:<64 lowercase hex> reference.
// A tag can move under the runner; a digest cannot, so only a digest makes
// "the same request ran the same runtime" a statement about the image rather
// than about when it was pulled.
func checkImageDigest(model, image string) error {
	repo, digest, ok := strings.Cut(image, "@")
	if !ok || repo == "" {
		return failf(CodeInvalidRuntimeConfig, PhaseValidate,
			"model %s has no digest-pinned image", model)
	}
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hex) != 64 {
		return failf(CodeInvalidRuntimeConfig, PhaseValidate,
			"model %s needs a sha256 digest of 64 lowercase hexadecimal characters", model)
	}
	for i := 0; i < len(hex); i++ {
		c := hex[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return failf(CodeInvalidRuntimeConfig, PhaseValidate,
				"model %s image digest is not lowercase hexadecimal", model)
		}
	}
	return nil
}

func checkCapabilities(model string, caps []string) error {
	seen := map[string]bool{}
	for _, c := range caps {
		if c == "ALL" {
			return failf(CodeInvalidRuntimeConfig, PhaseValidate,
				"model %s requests ALL capabilities", model)
		}
		if !capabilityAllowlist[c] {
			return failf(CodeInvalidRuntimeConfig, PhaseValidate,
				"model %s requests capability %q, which is not in the compiled allowlist", model, c)
		}
		if seen[c] {
			return failf(CodeInvalidRuntimeConfig, PhaseValidate,
				"model %s lists capability %s twice", model, c)
		}
		seen[c] = true
	}
	return nil
}
