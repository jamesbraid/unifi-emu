package firmware

import (
	"bytes"
	"strings"
	"testing"
)

func TestManifestWriteNormalizesOrderAndDuplicates(t *testing.T) {
	m := Manifest{
		SchemaVersion: 1,
		Images: []ImageEvidence{
			{
				SHA256:    strings.Repeat("b", 64),
				Platforms: []string{"USW", "UAP", "USW"},
				Observations: []Observation{
					{Kind: "capability", Value: "fw_caps", Artifact: "root", Offset: 20},
					{Kind: "command", Value: "setparam", Artifact: "root", Offset: 10},
					{Kind: "capability", Value: "fw_caps", Artifact: "root", Offset: 20},
				},
				Warnings: []string{"z warning", "a warning", "z warning"},
			},
			{SHA256: strings.Repeat("a", 64), Platforms: []string{"UXG"}},
		},
	}

	var first, second bytes.Buffer
	if err := WriteManifest(&first, m); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(&second, m); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatal("manifest output changed between writes")
	}

	got := first.String()
	if strings.Index(got, strings.Repeat("a", 64)) > strings.Index(got, strings.Repeat("b", 64)) {
		t.Fatal("images are not ordered by checksum")
	}
	if strings.Count(got, `"value": "fw_caps"`) != 1 {
		t.Fatalf("duplicate observation retained:\n%s", got)
	}
	if strings.Index(got, `"UAP"`) > strings.Index(got, `"USW"`) {
		t.Fatalf("platforms are not ordered:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatal("manifest is not newline terminated")
	}
}

func TestManifestWritePreservesStructuredFailures(t *testing.T) {
	m := Manifest{SchemaVersion: 1, Images: []ImageEvidence{{
		SHA256: strings.Repeat("c", 64),
		Failures: []Failure{
			{Stage: "decode", Artifact: "root/part-1", Error: "unsupported lzo stream"},
		},
	}}}
	var out bytes.Buffer
	if err := WriteManifest(&out, m); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"stage": "decode"`, `"artifact": "root/part-1"`, `"error": "unsupported lzo stream"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("manifest missing %s:\n%s", want, out.String())
		}
	}
}
