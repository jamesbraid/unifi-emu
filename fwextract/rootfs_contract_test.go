package fwextract

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

	entries, err := parseTar(archive.Bytes(), defaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "usr" ||
		entries[0].Kind != "directory" || entries[0].Mode != 0o750 {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestParseTarCanonicalizesLeadingDotSlashAndSkipsRoot(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, header := range []*tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./ingssvcd", Typeflag: tar.TypeReg, Mode: 0o755, Size: 3},
		{Name: "./AR1520A.img", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte("gps")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := parseTar(archive.Bytes(), defaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "ingssvcd" ||
		entries[1].Path != "AR1520A.img" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestParseTarStillRejectsTraversalBelowDotSlash(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name: "./../escape", Typeflag: tar.TypeReg, Mode: 0o644,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTar(archive.Bytes(), defaultLimits()); err == nil {
		t.Fatal("accepted tar traversal below ./")
	}
}

func TestParseTarPreservesDeviceNodesAndFIFO(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, header := range []*tar.Header{
		{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3},
		{Name: "dev/mtdblock0", Typeflag: tar.TypeBlock, Mode: 0o640, Devmajor: 31},
		{Name: "run/events", Typeflag: tar.TypeFifo, Mode: 0o620},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := parseTar(archive.Bytes(), defaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %+v", entries)
	}
	if got := entries[0]; got.Kind != "char-device" || got.DeviceMajor != 1 || got.DeviceMinor != 3 {
		t.Fatalf("character device = %+v", got)
	}
	if got := entries[1]; got.Kind != "block-device" || got.DeviceMajor != 31 || got.DeviceMinor != 0 {
		t.Fatalf("block device = %+v", got)
	}
	if got := entries[2]; got.Kind != "fifo" || got.Mode != 0o620 {
		t.Fatalf("FIFO = %+v", got)
	}
}

func TestParseTarRejectsUnsupportedEntryInsteadOfSkippingIt(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name: "run/control.sock", Typeflag: tar.TypeGNUSparse, Mode: 0o600,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTar(archive.Bytes(), defaultLimits()); err == nil {
		t.Fatal("unsupported tar entry was silently skipped")
	}
}

func TestCPIORootFSTarReconcilesSpecialEntriesAndDuplicates(t *testing.T) {
	var archive []byte
	archive = append(archive, newcEntryMeta("dev", nil, 0o040755, 1, 1, 0, 0)...)
	archive = append(archive, newcEntryDevice("dev/console", 0o020600, 5, 1)...)
	archive = append(archive, newcEntryDevice("dev/console", 0o020660, 5, 1)...)
	archive = append(archive, newcEntryDevice("dev/mtdblock0", 0o060640, 31, 0)...)
	archive = append(archive, newcEntryDevice("run/events", 0o010620, 0, 0)...)
	archive = append(archive, newcEntry("TRAILER!!!", nil, 0)...)

	source, err := parseCPIO(archive, defaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(source) != 5 {
		t.Fatalf("source entries = %d, want 5 including duplicate console", len(source))
	}
	bundle, err := buildRootFSBundleBytes(decodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/initramfs", Entries: source,
	}}}, Source{Platform: "TEST"})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Metadata.EntryCount != 4 {
		t.Fatalf("output entries = %d, want 4 unique source paths", bundle.Metadata.EntryCount)
	}
	reader := tar.NewReader(bytes.NewReader(bundle.Tar))
	var types []byte
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, header.Typeflag)
		if header.Name == "dev/console" &&
			(header.Mode != 0o660 || header.Devmajor != 5 || header.Devminor != 1) {
			t.Fatalf("last console entry was not preserved: %+v", header)
		}
	}
	wantTypes := []byte{tar.TypeDir, tar.TypeChar, tar.TypeBlock, tar.TypeFifo}
	if !bytes.Equal(types, wantTypes) {
		t.Fatalf("output entry types = %q, want %q", types, wantTypes)
	}
}

func TestRootFSTarEmitsDeviceNodesAndFIFO(t *testing.T) {
	result := decodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/rootfs",
		Entries: []decodedFile{
			{Path: "dev/null", Kind: "char-device", Mode: 0o666, DeviceMajor: 1, DeviceMinor: 3},
			{Path: "dev/mtdblock0", Kind: "block-device", Mode: 0o640, DeviceMajor: 31},
			{Path: "run/events", Kind: "fifo", Mode: 0o620},
		},
	}}}
	bundle, err := buildRootFSBundleBytes(result, Source{Platform: "TEST"})
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(bundle.Tar))
	want := []struct {
		name       string
		kind       byte
		mode       int64
		major, min int64
	}{
		{"dev/mtdblock0", tar.TypeBlock, 0o640, 31, 0},
		{"dev/null", tar.TypeChar, 0o666, 1, 3},
		{"run/events", tar.TypeFifo, 0o620, 0, 0},
	}
	for _, expected := range want {
		header, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != expected.name || header.Typeflag != expected.kind ||
			header.Mode != expected.mode || header.Devmajor != expected.major ||
			header.Devminor != expected.min {
			t.Fatalf("header = %+v, want %+v", header, expected)
		}
	}
}

func TestRootFSTarWritesHardlinkAfterItsTarget(t *testing.T) {
	result := decodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/rootfs",
		Entries: []decodedFile{
			{Path: "z-target", Kind: "regular", Mode: 0o640, Data: []byte("vendor")},
			{Path: "a-link", Kind: "hardlink", Mode: 0o640, Linkname: "z-target"},
		},
	}}}
	bundle, err := buildRootFSBundleBytes(result, Source{
		Digest: strings.Repeat("a", 64), Platform: "TEST", Version: "1",
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
	result := decodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/rootfs",
		Entries: []decodedFile{
			{Path: "z-target", Kind: "regular", Mode: 0o640, Data: []byte("vendor")},
			{Path: "a-link", Kind: "hardlink", Mode: 0o640, Linkname: "z-target"},
		},
	}}}
	bundle, err := buildRootFSBundleBytes(result, Source{
		Digest: strings.Repeat("a", 64), Platform: "TEST", Version: "1",
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
	result := decodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/rootfs",
		Entries: []decodedFile{
			{Path: "z-target", Kind: "regular", Mode: 0o4755, Data: []byte("vendor")},
			{Path: "a-link", Kind: "hardlink", Mode: 0, Linkname: "z-target"},
		},
	}}}
	bundle, err := buildRootFSBundleBytes(result, Source{Platform: "TEST"})
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

func TestNormalizeRootEntriesUsesNonEmptyHardlinkPayloadRegardlessOfPathOrder(t *testing.T) {
	entries, err := normalizeRootEntries([]decodedFile{
		{Path: "a-empty", Kind: "regular", Mode: 0o600, LinkKey: "inode:7"},
		{Path: "z-payload", Kind: "regular", Mode: 0o4755, LinkKey: "inode:7", Data: []byte("vendor")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 ||
		entries[0].Path != "a-empty" || entries[0].Kind != "regular" ||
		string(entries[0].Data) != "vendor" || entries[0].Mode != 0o4755 ||
		entries[1].Path != "z-payload" || entries[1].Kind != "hardlink" ||
		entries[1].Linkname != "a-empty" {
		t.Fatalf("normalized entries = %+v", entries)
	}
}

func TestNormalizeRootEntriesRejectsConflictingHardlinkPayloads(t *testing.T) {
	_, err := normalizeRootEntries([]decodedFile{
		{Path: "a", Kind: "regular", Mode: 0o755, LinkKey: "inode:7", Data: []byte("one")},
		{Path: "z", Kind: "regular", Mode: 0o755, LinkKey: "inode:7", Data: []byte("two")},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting payloads") {
		t.Fatalf("error = %v, want conflicting payloads", err)
	}
}

func TestNormalizeRootEntriesAllowsAllEmptyHardlinkGroup(t *testing.T) {
	entries, err := normalizeRootEntries([]decodedFile{
		{Path: "a", Kind: "regular", Mode: 0o644, LinkKey: "inode:7"},
		{Path: "z", Kind: "regular", Mode: 0o644, LinkKey: "inode:7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Kind != "regular" || len(entries[0].Data) != 0 ||
		entries[1].Kind != "hardlink" || entries[1].Linkname != "a" {
		t.Fatalf("normalized entries = %+v", entries)
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

func TestMetadataHasExactlySixFields(t *testing.T) {
	var output bytes.Buffer
	if err := WriteMetadata(&output, Metadata{
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
	result := decodeResult{Roots: []decodedRoot{{
		Artifact: "firmware.bin/rootfs",
		Entries: []decodedFile{
			{Path: "usr", Kind: "directory", Mode: 0o755},
			{Path: "usr/bin/mcad", Kind: "regular", Mode: 0o755, Data: []byte("vendor")},
		},
	}}}
	bundle, err := buildRootFSBundleBytes(result, Source{Platform: "TEST"})
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
