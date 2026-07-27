package firmware

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestAnalyzeImageBuildsEvidenceAndKeepsDecodeFailures(t *testing.T) {
	data := ubntImage(ubntPart("agent", []byte("fw_caps\x00setparam\x00")))
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	image := AnalyzeImage(SelectedImage{
		SHA256: sum, Version: "1.2.3", Platforms: []string{"TEST"},
		URL: "https://example/firmware.bin", Size: int64(len(data)),
	}, data, DefaultLimits())
	if image.Format != "ubnt" || image.SHA256 != sum || len(image.Artifacts) < 2 {
		t.Fatalf("image evidence = %+v", image)
	}
	if len(image.Observations) != 2 {
		t.Fatalf("observations = %+v", image.Observations)
	}

	broken := AnalyzeImage(SelectedImage{SHA256: sum}, data[:270], DefaultLimits())
	if len(broken.Failures) == 0 {
		t.Fatal("decode failure was discarded")
	}
}

func TestAttachLiveEvidenceRequiresOneImage(t *testing.T) {
	manifest := Manifest{Images: []ImageEvidence{{SHA256: "one"}, {SHA256: "two"}}}
	if err := AttachLiveEvidence(&manifest, strings.NewReader(`{"fw_caps":3}`)); err == nil {
		t.Fatal("attached one device dump to multiple firmware images")
	}
}
