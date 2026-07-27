package fwextract

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

type testRootFSBundle struct {
	Tar      []byte
	Metadata Metadata
}

func buildRootFSBundleBytes(decoded decodeResult, source Source) (testRootFSBundle, error) {
	var output bytes.Buffer
	metadata, err := buildRootFSBundle(decoded, source, &output)
	return testRootFSBundle{Tar: output.Bytes(), Metadata: metadata}, err
}

func TestBuildRootFSBundleIsDeterministicAndPreservesMetadata(t *testing.T) {
	result := decodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/kernel/initramfs",
		Entries: []decodedFile{
			{Path: "usr/bin/tool-link", Kind: "regular", Mode: 0o755, LinkKey: "inode:7"},
			{Path: "usr", Kind: "directory", Mode: 0o755},
			{Path: "usr/bin/tool", Kind: "regular", Mode: 0o755, LinkKey: "inode:7", Data: []byte("vendor")},
			{Path: "bin", Kind: "symlink", Mode: 0o777, Linkname: "usr/bin"},
		},
	}}}
	source := Source{
		Digest: strings.Repeat("a", 64), Version: "8.6.11", Platform: "U7PRO",
	}
	first, err := buildRootFSBundleBytes(result, source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildRootFSBundleBytes(result, source)
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

func TestSelectRootFSPrefersCompleteInitramfsOverFirmwarePayload(t *testing.T) {
	roots := []decodedRoot{
		{
			Artifact: "firmware.bin/kernel0/kernel/initramfs",
			Entries:  make([]decodedFile, 2002),
		},
		{
			Artifact: "firmware.bin/kernel0/kernel/initramfs/usr/share/xbl/xbl.tgz",
			Entries:  make([]decodedFile, 1894),
		},
		{
			Artifact: "firmware.bin/wififw",
			Entries:  make([]decodedFile, 184),
		},
	}

	selected, ok := selectRootFS(roots)
	if !ok {
		t.Fatal("no root filesystem selected")
	}
	if selected.Artifact != "firmware.bin/kernel0/kernel/initramfs" {
		t.Fatalf("selected root = %q", selected.Artifact)
	}
}
