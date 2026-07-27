// Package fwextract verifies a firmware image and extracts its root filesystem.
package fwextract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Source identifies the firmware bytes supplied to Extract.
type Source struct {
	Digest   string
	Platform string
	Version  string
}

// ErrNoRootFS reports that extraction found no supported root filesystem.
var ErrNoRootFS = errors.New("firmware contains no decoded root filesystem")

// Extract verifies image against source, writes its deterministic root
// filesystem tar to output, and returns the tar metadata.
func Extract(image []byte, source Source, output io.Writer) (Metadata, error) {
	if len(source.Digest) != sha256.Size*2 {
		return Metadata{}, fmt.Errorf("invalid source firmware digest %q: must be 64 hexadecimal characters", source.Digest)
	}
	if _, err := hex.DecodeString(source.Digest); err != nil {
		return Metadata{}, fmt.Errorf("invalid source firmware digest %q: %w", source.Digest, err)
	}
	if strings.TrimSpace(source.Platform) == "" {
		return Metadata{}, errors.New("source platform is required")
	}
	if strings.TrimSpace(source.Version) == "" {
		return Metadata{}, errors.New("source version is required")
	}
	source.Digest = strings.ToLower(source.Digest)
	actual := fmt.Sprintf("%x", sha256.Sum256(image))
	if actual != source.Digest {
		return Metadata{}, fmt.Errorf("firmware digest mismatch: got %s, want %s", actual, source.Digest)
	}
	decoded := decode(image, "firmware.bin", defaultLimits())
	metadata, err := buildRootFSBundle(decoded, source, output)
	if err == nil || len(decoded.Roots) > 0 || len(decoded.Failures) == 0 {
		return metadata, err
	}
	failures := make([]error, 1, len(decoded.Failures)+1)
	failures[0] = err
	for _, failure := range decoded.Failures {
		failures = append(failures, fmt.Errorf(
			"decode %s at %s: %s", failure.Stage, failure.Artifact, failure.Error,
		))
	}
	return Metadata{}, errors.Join(failures...)
}
