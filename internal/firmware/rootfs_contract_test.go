package firmware

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestParseTarAcceptsCanonicalDirectoryNames(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o750,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := parseTar(archive.Bytes(), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "usr" ||
		entries[0].Kind != "directory" || entries[0].Mode != 0o750 {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestRootFSTarWritesHardlinkAfterItsTarget(t *testing.T) {
	result := DecodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/rootfs",
		Format:   "tar",
		Entries: []decodedFile{
			{Path: "z-target", Kind: "regular", Mode: 0o640, Data: []byte("vendor")},
			{Path: "a-link", Kind: "hardlink", Mode: 0o640, Linkname: "z-target"},
		},
	}}}
	bundle, err := buildRootFSBundle(result, SelectedImage{
		SHA256: strings.Repeat("a", 64), Platforms: []string{"TEST"}, Version: "1",
	})
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	reader := tar.NewReader(bytes.NewReader(bundle.Tar))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeLink && !seen[header.Linkname] {
			t.Fatalf("hardlink %q precedes target %q", header.Name, header.Linkname)
		}
		seen[header.Name] = true
	}
}

func TestRootFSTarKeepsCanonicalPathOrderWithHardlinks(t *testing.T) {
	result := DecodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/rootfs",
		Format:   "tar",
		Entries: []decodedFile{
			{Path: "z-target", Kind: "regular", Mode: 0o640, Data: []byte("vendor")},
			{Path: "a-link", Kind: "hardlink", Mode: 0o640, Linkname: "z-target"},
		},
	}}}
	bundle, err := buildRootFSBundle(result, SelectedImage{
		SHA256: strings.Repeat("a", 64), Platforms: []string{"TEST"}, Version: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(bundle.Tar))
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
	if got := strings.Join(names, ","); got != "a-link,z-target" {
		t.Fatalf("rootfs tar path order = %q", got)
	}
}

func TestRootFSTarCanonicalHardlinkKeepsPayloadMode(t *testing.T) {
	result := DecodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/rootfs",
		Format:   "tar",
		Entries: []decodedFile{
			{Path: "z-target", Kind: "regular", Mode: 0o4755, Data: []byte("vendor")},
			{Path: "a-link", Kind: "hardlink", Mode: 0, Linkname: "z-target"},
		},
	}}}
	bundle, err := buildRootFSBundle(result, SelectedImage{Platforms: []string{"TEST"}})
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(bundle.Tar))
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "a-link" || header.Typeflag != tar.TypeReg || header.Mode != 0o4755 {
		t.Fatalf("canonical hardlink header = %+v", header)
	}
}

func TestNormalizeRootEntriesRejectsChildBelowSymlink(t *testing.T) {
	_, err := normalizeRootEntries([]decodedFile{
		{Path: "etc", Kind: "symlink", Mode: 0o777, Linkname: "/host/etc"},
		{Path: "etc/passwd", Kind: "regular", Mode: 0o644, Data: []byte("vendor")},
	})
	if err == nil {
		t.Fatal("accepted a file below a symlink path")
	}
}

func TestNormalizeRootEntriesRejectsChildBelowRegularFile(t *testing.T) {
	_, err := normalizeRootEntries([]decodedFile{
		{Path: "etc", Kind: "regular", Mode: 0o644, Data: []byte("vendor")},
		{Path: "etc/passwd", Kind: "regular", Mode: 0o644, Data: []byte("vendor")},
	})
	if err == nil {
		t.Fatal("accepted a file below a regular-file path")
	}
}

func TestNormalizeRootEntriesUsesLastEntryExactly(t *testing.T) {
	entries, err := normalizeRootEntries([]decodedFile{
		{Path: "usr/bin/tool", Kind: "regular", Mode: 0o755, Data: []byte("old")},
		{Path: "usr/bin/tool", Kind: "symlink", Mode: 0o777, Linkname: "../lib/tool"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != "symlink" ||
		entries[0].Mode != 0o777 || entries[0].Linkname != "../lib/tool" ||
		len(entries[0].Data) != 0 {
		t.Fatalf("last entry was not preserved exactly: %+v", entries)
	}
}

func TestRootFSMetadataHasExactlySixFields(t *testing.T) {
	var output bytes.Buffer
	if err := WriteRootFSMetadata(&output, RootFSMetadata{
		SourceFirmwareDigest: strings.Repeat("a", 64),
		Platform:             "TEST",
		Version:              "1",
		NestedArtifactPath:   "firmware.bin/rootfs",
		TarDigest:            strings.Repeat("b", 64),
		EntryCount:           3,
	}); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"source_firmware_digest", "platform", "version",
		"nested_artifact_path", "tar_digest", "entry_count",
	}
	if len(fields) != len(want) {
		t.Fatalf("rootfs metadata fields = %v", fields)
	}
	for _, name := range want {
		if _, ok := fields[name]; !ok {
			t.Errorf("rootfs metadata missing %q", name)
		}
	}
}

func TestRootFSTarContainsOnlyVendorEntries(t *testing.T) {
	result := DecodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/rootfs",
		Entries: []decodedFile{
			{Path: "usr", Kind: "directory", Mode: 0o755},
			{Path: "usr/bin/mcad", Kind: "regular", Mode: 0o755, Data: []byte("vendor")},
		},
	}}}
	bundle, err := buildRootFSBundle(result, SelectedImage{Platforms: []string{"TEST"}})
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(bundle.Tar))
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
		t.Fatalf("rootfs tar entries = %q", got)
	}
}
