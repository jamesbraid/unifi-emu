package firmware

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestBuildRootFSBundleIsDeterministicAndPreservesMetadata(t *testing.T) {
	result := DecodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/kernel/initramfs",
		Format:   "cpio",
		Entries: []decodedFile{
			{Path: "usr/bin/tool-link", Kind: "regular", Mode: 0o755, LinkKey: "inode:7"},
			{Path: "usr", Kind: "directory", Mode: 0o755},
			{Path: "usr/bin/tool", Kind: "regular", Mode: 0o755, LinkKey: "inode:7", Data: []byte("vendor")},
			{Path: "bin", Kind: "symlink", Mode: 0o777, Linkname: "usr/bin"},
		},
	}}}
	image := SelectedImage{
		SHA256: strings.Repeat("a", 64), Version: "8.6.11", Platforms: []string{"U7PRO"},
	}
	first, err := buildRootFSBundle(result, image)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildRootFSBundle(result, image)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Tar, second.Tar) {
		t.Fatal("rootfs tar is not deterministic")
	}
	if first.Metadata.TarDigest != fmt.Sprintf("%x", sha256.Sum256(first.Tar)) ||
		first.Metadata.EntryCount != 4 || first.Metadata.NestedArtifactPath != result.Roots[0].Artifact {
		t.Fatalf("metadata = %+v", first.Metadata)
	}

	reader := tar.NewReader(bytes.NewReader(first.Tar))
	entries := map[string]*tar.Header{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		clone := *header
		entries[header.Name] = &clone
		if !header.ModTime.Equal(time.Unix(0, 0)) {
			t.Fatalf("%s timestamp = %v", header.Name, header.ModTime)
		}
	}
	if entries["usr/"].Typeflag != tar.TypeDir ||
		entries["bin"].Typeflag != tar.TypeSymlink || entries["bin"].Linkname != "usr/bin" ||
		entries["usr/bin/tool-link"].Typeflag != tar.TypeLink ||
		entries["usr/bin/tool-link"].Linkname != "usr/bin/tool" ||
		entries["usr/bin/tool"].Mode != 0o755 {
		t.Fatalf("tar entries = %+v", entries)
	}
}
