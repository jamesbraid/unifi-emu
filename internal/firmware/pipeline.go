package firmware

import (
	"fmt"
	"io"
)

func AnalyzeImage(image SelectedImage, data []byte, limits Limits) ImageEvidence {
	evidence, _, _ := AnalyzeImageWithRootFS(image, data, limits)
	return evidence
}

func AnalyzeImageWithRootFS(image SelectedImage, data []byte, limits Limits) (ImageEvidence, *RootFSBundle, error) {
	result := Decode(data, "firmware.bin", limits)
	evidence := ImageEvidence{
		SHA256: image.SHA256, Version: image.Version,
		Platforms: append([]string(nil), image.Platforms...),
		SourceURL: image.URL, SourceURLs: append([]string(nil), image.URLs...),
		Size: int64(len(data)), Format: detectFormat(data),
		Artifacts: result.Artifacts, Warnings: result.Warnings, Failures: result.Failures,
	}
	evidence.Observations = Analyze(result.Files, limits.withDefaults().MinStringLength)
	rootfs, err := buildRootFSBundle(result, image)
	if err != nil {
		return evidence, nil, err
	}
	return evidence, rootfs, nil
}

func AttachLiveEvidence(manifest *Manifest, r io.Reader) error {
	if len(manifest.Images) != 1 {
		return fmt.Errorf("live evidence requires exactly one firmware image, got %d", len(manifest.Images))
	}
	live, fields, err := ParseLiveSnapshot(r)
	if err != nil {
		return err
	}
	manifest.LiveEvidence = live
	manifest.LiveFields = fields
	comparison := CompareEvidence(manifest.Images[0].Observations, live)
	manifest.Comparison = &comparison
	return nil
}

func AttachCapabilityDefinitions(manifest *Manifest, r io.Reader) error {
	if len(manifest.LiveFields) == 0 {
		return fmt.Errorf("capability decoding requires JSON live evidence")
	}
	definitions, err := ParseCapabilityDefinitions(r)
	if err != nil {
		return err
	}
	manifest.CapabilityValues = DecodeCapabilityValues(manifest.LiveFields, definitions)
	return nil
}
