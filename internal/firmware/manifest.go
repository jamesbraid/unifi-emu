// Package firmware extracts auditable evidence from UniFi firmware images.
package firmware

import (
	"encoding/json"
	"io"
	"slices"
)

type Manifest struct {
	SchemaVersion    int                 `json:"schema_version"`
	Generator        string              `json:"generator,omitempty"`
	GeneratorVersion string              `json:"generator_version,omitempty"`
	Limits           ManifestLimits      `json:"limits"`
	Images           []ImageEvidence     `json:"images"`
	LiveEvidence     []Observation       `json:"live_evidence,omitempty"`
	LiveFields       []LiveField         `json:"live_fields,omitempty"`
	CapabilityValues []CapabilityValue   `json:"capability_values,omitempty"`
	Comparison       *EvidenceComparison `json:"comparison,omitempty"`
}

type ManifestLimits struct {
	MaxImageBytes    int64 `json:"max_image_bytes"`
	MaxExpandedBytes int64 `json:"max_expanded_bytes"`
	MaxArtifacts     int   `json:"max_artifacts"`
	MaxDepth         int   `json:"max_depth"`
	MinStringLength  int   `json:"min_string_length"`
}

type ImageEvidence struct {
	SHA256       string        `json:"sha256"`
	Version      string        `json:"version,omitempty"`
	Platforms    []string      `json:"platforms,omitempty"`
	SourceURL    string        `json:"source_url,omitempty"`
	SourceURLs   []string      `json:"source_urls,omitempty"`
	Size         int64         `json:"size,omitempty"`
	Format       string        `json:"format,omitempty"`
	Artifacts    []Artifact    `json:"artifacts,omitempty"`
	Observations []Observation `json:"observations,omitempty"`
	Warnings     []string      `json:"warnings,omitempty"`
	Failures     []Failure     `json:"failures,omitempty"`
}

type Artifact struct {
	Path        string `json:"path"`
	Format      string `json:"format"`
	Offset      int64  `json:"offset,omitempty"`
	OffsetBasis string `json:"offset_basis,omitempty"`
	Size        int64  `json:"size"`
	Compression string `json:"compression,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
}

type Observation struct {
	Kind          string `json:"kind"`
	Value         string `json:"value"`
	ObservedValue string `json:"observed_value,omitempty"`
	Artifact      string `json:"artifact"`
	Offset        int64  `json:"offset"`
	OffsetBasis   string `json:"offset_basis,omitempty"`
	Confidence    string `json:"confidence,omitempty"`
	Source        string `json:"source,omitempty"`
	Context       string `json:"context,omitempty"`
}

type LiveField struct {
	Path  string `json:"path"`
	Type  string `json:"type"`
	Value string `json:"value,omitempty"`
}

type Failure struct {
	Stage    string `json:"stage"`
	Artifact string `json:"artifact,omitempty"`
	Error    string `json:"error"`
}

func WriteManifest(w io.Writer, manifest Manifest) error {
	normalizeManifest(&manifest)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(manifest)
}

func normalizeManifest(m *Manifest) {
	slices.SortFunc(m.Images, func(a, b ImageEvidence) int {
		return cmpStrings(a.SHA256, b.SHA256)
	})
	for i := range m.Images {
		image := &m.Images[i]
		image.Platforms = uniqueStrings(image.Platforms)
		image.SourceURLs = uniqueStrings(image.SourceURLs)
		image.Warnings = uniqueStrings(image.Warnings)
		slices.SortFunc(image.Artifacts, func(a, b Artifact) int {
			if n := cmpStrings(a.Path, b.Path); n != 0 {
				return n
			}
			return cmpInt64(a.Offset, b.Offset)
		})
		slices.SortFunc(image.Failures, func(a, b Failure) int {
			if n := cmpStrings(a.Stage, b.Stage); n != 0 {
				return n
			}
			if n := cmpStrings(a.Artifact, b.Artifact); n != 0 {
				return n
			}
			return cmpStrings(a.Error, b.Error)
		})
		slices.SortFunc(image.Observations, compareObservation)
		image.Observations = slices.Compact(image.Observations)
	}
	slices.SortFunc(m.LiveEvidence, compareObservation)
	m.LiveEvidence = slices.Compact(m.LiveEvidence)
	slices.SortFunc(m.LiveFields, func(a, b LiveField) int { return cmpStrings(a.Path, b.Path) })
	slices.SortFunc(m.CapabilityValues, func(a, b CapabilityValue) int {
		if n := cmpStrings(a.Path, b.Path); n != 0 {
			return n
		}
		return cmpStrings(a.Bitmap, b.Bitmap)
	})
}

func compareObservation(a, b Observation) int {
	if n := cmpStrings(a.Kind, b.Kind); n != 0 {
		return n
	}
	if n := cmpStrings(a.Value, b.Value); n != 0 {
		return n
	}
	if n := cmpStrings(a.ObservedValue, b.ObservedValue); n != 0 {
		return n
	}
	if n := cmpStrings(a.Artifact, b.Artifact); n != 0 {
		return n
	}
	if n := cmpInt64(a.Offset, b.Offset); n != 0 {
		return n
	}
	if n := cmpStrings(a.OffsetBasis, b.OffsetBasis); n != 0 {
		return n
	}
	if n := cmpStrings(a.Source, b.Source); n != 0 {
		return n
	}
	if n := cmpStrings(a.Context, b.Context); n != 0 {
		return n
	}
	return cmpStrings(a.Confidence, b.Confidence)
}

func uniqueStrings(values []string) []string {
	slices.Sort(values)
	return slices.Compact(values)
}

func cmpStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
