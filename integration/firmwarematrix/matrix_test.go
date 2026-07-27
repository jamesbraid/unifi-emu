package firmwarematrix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/fwextract"
	"github.com/jamesbraid/unifi-emu/internal/fwgrab"
)

const (
	outcomeRootFS   = "rootfs"
	outcomeNoRootFS = "no_rootfs"
)

type manifest struct {
	SchemaVersion int       `json:"schema_version"`
	Images        []fixture `json:"images"`
}

type fixture struct {
	Platform           string `json:"platform"`
	Version            string `json:"version"`
	SourceURL          string `json:"source_url"`
	SourceSHA256       string `json:"source_sha256"`
	Outcome            string `json:"outcome"`
	RequiresLZOP       bool   `json:"requires_lzop,omitempty"`
	NestedArtifactPath string `json:"nested_artifact_path"`
	TarSHA256          string `json:"tar_sha256"`
	EntryCount         int    `json:"entry_count"`
}

func TestManifest(t *testing.T) {
	m := loadManifest(t)
	if err := validateManifest(m); err != nil {
		t.Fatal(err)
	}
}

func TestValidateManifestRejectsOutcomeFieldMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fixture)
	}{
		{
			name: "rootfs without receipt",
			mutate: func(image *fixture) {
				image.TarSHA256 = ""
			},
		},
		{
			name: "no rootfs with receipt",
			mutate: func(image *fixture) {
				image.Outcome = outcomeNoRootFS
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := loadManifest(t)
			test.mutate(&m.Images[0])
			if err := validateManifest(m); err == nil {
				t.Fatal("validateManifest succeeded, want outcome field mismatch")
			}
		})
	}
}

func TestRealFirmwareMatrix(t *testing.T) {
	if os.Getenv("FWEXTRACT_FIRMWARE_MATRIX") != "1" {
		t.Skip("set FWEXTRACT_FIRMWARE_MATRIX=1 to download and extract the real-firmware matrix")
	}
	m := loadManifest(t)
	if err := validateManifest(m); err != nil {
		t.Fatal(err)
	}
	cacheDir := os.Getenv("FWEXTRACT_FIRMWARE_CACHE")
	if cacheDir == "" {
		userCacheDir, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		cacheDir = filepath.Join(userCacheDir, "fwextract", "firmwarematrix")
	}
	cache := fwgrab.Cache{Dir: cacheDir}
	for _, image := range m.Images {
		image := image
		t.Run(image.Platform, func(t *testing.T) {
			if image.RequiresLZOP {
				if _, err := exec.LookPath("lzop"); err != nil {
					t.Fatal("this firmware uses lzop framing; install the lzop executable and retry")
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
			defer cancel()
			path, err := cache.Fetch(ctx, fwgrab.SelectedImage{
				SHA256:    image.SourceSHA256,
				Version:   image.Version,
				Platforms: []string{image.Platform},
				URL:       image.SourceURL,
				URLs:      []string{image.SourceURL},
			})
			if err != nil {
				t.Fatalf("acquire verified firmware: %v", err)
			}
			firmware, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read cached firmware: %v", err)
			}
			for run := 1; run <= 2; run++ {
				assertExtraction(t, image, firmware, run)
			}
		})
	}
}

func assertExtraction(t *testing.T, image fixture, firmware []byte, run int) {
	t.Helper()
	hasher := sha256.New()
	metadata, err := fwextract.Extract(firmware, fwextract.Source{
		Digest: image.SourceSHA256, Platform: image.Platform, Version: image.Version,
	}, hasher)
	if image.Outcome == outcomeNoRootFS {
		if !errors.Is(err, fwextract.ErrNoRootFS) {
			t.Fatalf("run %d: error = %v, want errors.Is(ErrNoRootFS)", run, err)
		}
		return
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "lzop executable is required") {
			t.Fatalf("run %d: extraction needs the lzop executable; install lzop and retry: %v", run, err)
		}
		t.Fatalf("run %d: extract: %v", run, err)
	}
	actualTarSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if actualTarSHA256 != image.TarSHA256 {
		t.Errorf("run %d: streamed tar SHA-256 = %s, want %s", run, actualTarSHA256, image.TarSHA256)
	}
	if metadata.SourceFirmwareDigest != image.SourceSHA256 {
		t.Errorf("run %d: source digest = %s, want %s", run, metadata.SourceFirmwareDigest, image.SourceSHA256)
	}
	if metadata.Platform != image.Platform {
		t.Errorf("run %d: platform = %s, want %s", run, metadata.Platform, image.Platform)
	}
	if metadata.Version != image.Version {
		t.Errorf("run %d: version = %s, want %s", run, metadata.Version, image.Version)
	}
	if metadata.NestedArtifactPath != image.NestedArtifactPath {
		t.Errorf("run %d: nested artifact = %s, want %s", run, metadata.NestedArtifactPath, image.NestedArtifactPath)
	}
	if metadata.TarDigest != image.TarSHA256 {
		t.Errorf("run %d: metadata tar digest = %s, want %s", run, metadata.TarDigest, image.TarSHA256)
	}
	if metadata.EntryCount != image.EntryCount {
		t.Errorf("run %d: entry count = %d, want %d", run, metadata.EntryCount, image.EntryCount)
	}
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	data, err := os.ReadFile("matrix.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		t.Fatalf("decode matrix.json: %v", err)
	}
	return m
}

func validateManifest(m manifest) error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("schema_version = %d, want 1", m.SchemaVersion)
	}
	if len(m.Images) != 20 {
		return fmt.Errorf("image count = %d, want 20", len(m.Images))
	}
	seen := make(map[string]bool, len(m.Images))
	for index, image := range m.Images {
		name := fmt.Sprintf("images[%d]", index)
		if strings.TrimSpace(image.Platform) == "" {
			return fmt.Errorf("%s: platform is required", name)
		}
		if strings.TrimSpace(image.Version) == "" {
			return fmt.Errorf("%s: version is required", name)
		}
		if !strings.HasPrefix(image.SourceURL, "https://") {
			return fmt.Errorf("%s: source_url must use https", name)
		}
		if !validSHA256(image.SourceSHA256) {
			return fmt.Errorf("%s: invalid source_sha256", name)
		}
		key := image.Platform + "\x00" + image.SourceSHA256
		if seen[key] {
			return fmt.Errorf("%s: duplicate platform and source_sha256", name)
		}
		seen[key] = true
		switch image.Outcome {
		case outcomeRootFS:
			if image.NestedArtifactPath == "" {
				return fmt.Errorf("%s: nested_artifact_path is required for rootfs", name)
			}
			if !validSHA256(image.TarSHA256) {
				return fmt.Errorf("%s: invalid tar_sha256 for rootfs", name)
			}
			if image.EntryCount <= 0 {
				return fmt.Errorf("%s: entry_count must be positive for rootfs", name)
			}
		case outcomeNoRootFS:
			if image.RequiresLZOP {
				return fmt.Errorf("%s: no_rootfs cannot require lzop", name)
			}
			if image.NestedArtifactPath != "" || image.TarSHA256 != "" || image.EntryCount != 0 {
				return fmt.Errorf("%s: no_rootfs must have empty artifact and tar digests and zero entries", name)
			}
		default:
			return fmt.Errorf("%s: unknown outcome %q", name, image.Outcome)
		}
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
