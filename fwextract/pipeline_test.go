package fwextract

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestExtractReturnsDeterministicRootFSContract(t *testing.T) {
	image := extractionTestImage()
	digest := fmt.Sprintf("%x", sha256.Sum256(image))
	source := Source{Digest: strings.ToUpper(digest), Platform: "TEST", Version: "1.2.3"}

	var firstTar bytes.Buffer
	first, err := Extract(image, source, &firstTar)
	if err != nil {
		t.Fatal(err)
	}
	var secondTar bytes.Buffer
	second, err := Extract(image, source, &secondTar)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstTar.Bytes(), secondTar.Bytes()) {
		t.Fatal("rootfs tar is not deterministic")
	}
	if first != second {
		t.Fatalf("metadata differs: %+v / %+v", first, second)
	}
	if first.SourceFirmwareDigest != digest ||
		first.Platform != "TEST" ||
		first.Version != "1.2.3" ||
		first.NestedArtifactPath != "firmware.bin" ||
		first.TarDigest != fmt.Sprintf("%x", sha256.Sum256(firstTar.Bytes())) ||
		first.EntryCount != 2 {
		t.Fatalf("metadata = %+v", first)
	}

	reader := tar.NewReader(bytes.NewReader(firstTar.Bytes()))
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	if got := strings.Join(names, ","); got != "usr/,usr/bin/mcad" {
		t.Fatalf("rootfs entries = %q", got)
	}
}

func TestExtractRejectsFirmwareDigestMismatch(t *testing.T) {
	image := extractionTestImage()
	_, err := Extract(image, Source{
		Digest: strings.Repeat("0", 64), Platform: "TEST", Version: "1",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "firmware digest mismatch") {
		t.Fatalf("digest error = %v", err)
	}
}

func TestExtractRejectsInvalidSource(t *testing.T) {
	image := extractionTestImage()
	digest := fmt.Sprintf("%x", sha256.Sum256(image))
	for name, tc := range map[string]struct {
		source Source
		want   string
	}{
		"empty digest":   {Source{Platform: "TEST", Version: "1"}, "invalid source firmware digest"},
		"short digest":   {Source{Digest: "abcd", Platform: "TEST", Version: "1"}, "invalid source firmware digest"},
		"non-hex digest": {Source{Digest: strings.Repeat("z", 64), Platform: "TEST", Version: "1"}, "invalid source firmware digest"},
		"empty platform": {Source{Digest: digest, Version: "1"}, "platform"},
		"blank platform": {Source{Digest: digest, Platform: " \t", Version: "1"}, "platform"},
		"empty version":  {Source{Digest: digest, Platform: "TEST"}, "version"},
		"blank version":  {Source{Digest: digest, Platform: "TEST", Version: "\n"}, "version"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Extract(image, tc.source, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExtractReturnsRootFilesystemError(t *testing.T) {
	image := []byte("not a firmware filesystem")
	digest := fmt.Sprintf("%x", sha256.Sum256(image))
	_, err := Extract(image, Source{Digest: digest, Platform: "TEST", Version: "1"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no decoded root filesystem") {
		t.Fatalf("rootfs error = %v", err)
	}
	if !errors.Is(err, ErrNoRootFS) {
		t.Fatalf("rootfs error = %v, want errors.Is(ErrNoRootFS)", err)
	}
}

func TestExtractPreservesDecoderFailureWhenNoRootExists(t *testing.T) {
	image := []byte("070701short")
	digest := fmt.Sprintf("%x", sha256.Sum256(image))
	_, err := Extract(image, Source{Digest: digest, Platform: "TEST", Version: "1"}, io.Discard)
	if err == nil {
		t.Fatal("accepted malformed CPIO")
	}
	for _, want := range []string{"no decoded root filesystem", "cpio", "firmware.bin", "CPIO"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestExtractIgnoresSiblingDecoderFailureWhenRootExists(t *testing.T) {
	good := extractionTestImage()
	records := append(ubntPart("broken", []byte("070701short")), ubntPart("rootfs", good)...)
	image := ubntImage(records)
	digest := fmt.Sprintf("%x", sha256.Sum256(image))

	var output bytes.Buffer
	metadata, err := Extract(image, Source{Digest: digest, Platform: "TEST", Version: "1"}, &output)
	if err != nil {
		t.Fatalf("valid root was rejected because a sibling failed: %v", err)
	}
	if metadata.NestedArtifactPath != "firmware.bin/rootfs" {
		t.Fatalf("nested artifact = %q", metadata.NestedArtifactPath)
	}
}

func extractionTestImage() []byte {
	var image []byte
	image = append(image, newcEntry("usr", nil, 0o040755)...)
	image = append(image, newcEntry("usr/bin/mcad", []byte("vendor"), 0o100755)...)
	return append(image, newcEntry("TRAILER!!!", nil, 0)...)
}
