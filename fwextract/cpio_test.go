package fwextract

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestParseCPIONewcReturnsRegularFiles(t *testing.T) {
	archive := append(newcEntry("usr/bin/mcad", []byte("agent"), 0o100755), newcEntry("TRAILER!!!", nil, 0)...)
	files, err := parseCPIO(archive, limits{MaxArtifacts: 10, MaxExpandedBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "usr/bin/mcad" ||
		!bytes.Equal(files[0].Data, []byte("agent")) {
		t.Fatalf("files = %+v", files)
	}
}

func TestParseCPIOConcatenatesMembersAcrossNULPadding(t *testing.T) {
	first := append(newcEntry("kernel/x86/microcode/GenuineIntel.bin", []byte("microcode"), 0o100644),
		newcEntry("TRAILER!!!", nil, 0)...)
	padding := make([]byte, 512-len(first)%512)
	second := append(newcEntry("usr/bin/mcad", []byte("agent"), 0o100755),
		newcEntry("TRAILER!!!", nil, 0)...)
	archive := append(append(first, padding...), second...)

	files, err := parseCPIO(archive, limits{MaxArtifacts: 10, MaxExpandedBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 ||
		files[0].Path != "kernel/x86/microcode/GenuineIntel.bin" ||
		files[1].Path != "usr/bin/mcad" ||
		string(files[1].Data) != "agent" {
		t.Fatalf("concatenated files = %+v", files)
	}
}

func TestParseCPIOConcatenatedMembersPreserveLastEntryOrder(t *testing.T) {
	first := append(newcEntry("etc/version", []byte("old"), 0o100644),
		newcEntry("TRAILER!!!", nil, 0)...)
	second := append(newcEntry("etc/version", []byte("new"), 0o100600),
		newcEntry("TRAILER!!!", nil, 0)...)

	files, err := parseCPIO(append(first, second...),
		limits{MaxArtifacts: 10, MaxExpandedBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeRootEntries(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 1 || normalized[0].Path != "etc/version" ||
		string(normalized[0].Data) != "new" || normalized[0].Mode != 0o600 {
		t.Fatalf("normalized files = %+v", normalized)
	}
}

func TestParseCPIORejectsNonNULTrailingData(t *testing.T) {
	archive := append(newcEntry("usr/bin/mcad", []byte("agent"), 0o100755),
		newcEntry("TRAILER!!!", nil, 0)...)
	archive = append(archive, []byte("not-cpio")...)

	_, err := parseCPIO(archive, limits{MaxArtifacts: 10, MaxExpandedBytes: 100})
	if err == nil || !strings.Contains(err.Error(), "non-NUL data after CPIO trailer") {
		t.Fatalf("parseCPIO error = %v, want trailing-data rejection", err)
	}
}

func TestPreflightCPIODescribesValidatedRecords(t *testing.T) {
	archive := append(newcEntry("usr/bin/mcad", []byte("agent"), 0o100755),
		newcEntry("TRAILER!!!", nil, 0)...)

	records, err := preflightCPIO(archive, limits{MaxArtifacts: 10, MaxExpandedBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "usr/bin/mcad" || records[0].Offset != 0 {
		t.Fatalf("records = %+v", records)
	}
}

func TestParseCPIOPreservesFilesystemMetadata(t *testing.T) {
	var archive []byte
	archive = append(archive, newcEntryMeta("usr", nil, 0o040750, 2, 1, 8, 1)...)
	archive = append(archive, newcEntryMeta("usr/bin/tool", []byte("tool"), 0o104755, 3, 1, 8, 1)...)
	archive = append(archive, newcEntryMeta("usr/bin/tool-link", []byte("usr/bin/tool\x00"), 0o120777, 4, 1, 8, 1)...)
	archive = append(archive, newcEntryMeta("usr/bin/hard-a", []byte("same"), 0o100644, 42, 2, 8, 1)...)
	archive = append(archive, newcEntryMeta("usr/bin/hard-b", nil, 0o100644, 42, 2, 8, 1)...)
	archive = append(archive, newcEntry("TRAILER!!!", nil, 0)...)

	files, err := parseCPIO(archive, limits{MaxArtifacts: 10, MaxExpandedBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Fatalf("entries = %+v, want directory, file, symlink, and two hardlinks", files)
	}
	byPath := make(map[string]decodedFile, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	if got := byPath["usr"]; got.Kind != "directory" || got.Mode != 0o750 {
		t.Fatalf("directory metadata = %+v", got)
	}
	if got := byPath["usr/bin/tool"]; got.Kind != "regular" || got.Mode != 0o4755 ||
		!bytes.Equal(got.Data, []byte("tool")) {
		t.Fatalf("regular metadata = %+v", got)
	}
	if got := byPath["usr/bin/tool-link"]; got.Kind != "symlink" || got.Mode != 0o777 ||
		got.Linkname != "usr/bin/tool" || len(got.Data) != 0 {
		t.Fatalf("symlink metadata = %+v", got)
	}
	hardA, hardB := byPath["usr/bin/hard-a"], byPath["usr/bin/hard-b"]
	if hardA.Kind != "regular" || hardB.Kind != "regular" ||
		hardA.LinkKey != "cpio:8:1:42" || hardB.LinkKey != hardA.LinkKey {
		t.Fatalf("hardlink metadata = %+v / %+v", hardA, hardB)
	}
}

func TestParseCPIOPreservesDeviceNodesAndFIFO(t *testing.T) {
	var archive []byte
	archive = append(archive, newcEntryDevice("dev/null", 0o020666, 1, 3)...)
	archive = append(archive, newcEntryDevice("dev/mtdblock0", 0o060640, 31, 0)...)
	archive = append(archive, newcEntryDevice("run/events", 0o010620, 0, 0)...)
	archive = append(archive, newcEntry("TRAILER!!!", nil, 0)...)

	files, err := parseCPIO(archive, limits{MaxArtifacts: 10, MaxExpandedBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("entries = %+v", files)
	}
	if got := files[0]; got.Path != "dev/null" || got.Kind != "char-device" ||
		got.Mode != 0o666 || got.DeviceMajor != 1 || got.DeviceMinor != 3 {
		t.Fatalf("character device = %+v", got)
	}
	if got := files[1]; got.Path != "dev/mtdblock0" || got.Kind != "block-device" ||
		got.Mode != 0o640 || got.DeviceMajor != 31 || got.DeviceMinor != 0 {
		t.Fatalf("block device = %+v", got)
	}
	if got := files[2]; got.Path != "run/events" || got.Kind != "fifo" || got.Mode != 0o620 {
		t.Fatalf("FIFO = %+v", got)
	}
}

func TestParseCPIORejectsSocketInsteadOfSkippingIt(t *testing.T) {
	archive := append(newcEntryDevice("run/control.sock", 0o140600, 0, 0),
		newcEntry("TRAILER!!!", nil, 0)...)
	_, err := parseCPIO(archive, limits{MaxArtifacts: 10, MaxExpandedBytes: 100})
	if err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("socket error = %v", err)
	}
}

func TestParseCPIORequiresTrailer(t *testing.T) {
	archive := newcEntry("usr/bin/mcad", []byte("agent"), 0o100755)
	if _, err := parseCPIO(archive, limits{MaxArtifacts: 10, MaxExpandedBytes: 100}); err == nil {
		t.Fatal("accepted CPIO archive without TRAILER!!!")
	}
}

func TestParseCPIORejectsUnsafeOrUnboundedArchives(t *testing.T) {
	crcArchive := append(newcEntry("one", []byte("x"), 0o100644), newcEntry("TRAILER!!!", nil, 0)...)
	copy(crcArchive[:6], "070702")
	oversizedName := newcEntry("one", []byte("x"), 0o100644)
	copy(oversizedName[94:102], "00100001")
	entry := newcEntry("one", []byte("x"), 0o100644)
	truncatedPadding := append(append([]byte(nil), entry[:len(entry)-1]...), newcEntry("TRAILER!!!", nil, 0)...)

	tests := map[string]struct {
		data   []byte
		limits limits
		want   string
	}{
		"traversal": {
			data:   append(newcEntry("../mcad", []byte("x"), 0o100644), newcEntry("TRAILER!!!", nil, 0)...),
			limits: limits{MaxArtifacts: 10, MaxExpandedBytes: 100},
		},
		"absolute": {
			data:   append(newcEntry("/bin/mcad", []byte("x"), 0o100644), newcEntry("TRAILER!!!", nil, 0)...),
			limits: limits{MaxArtifacts: 10, MaxExpandedBytes: 100},
		},
		"entries": {
			data: append(
				append(newcEntry("one", []byte("x"), 0o100644), newcEntry("two", []byte("y"), 0o100644)...),
				newcEntry("TRAILER!!!", nil, 0)...,
			),
			limits: limits{MaxArtifacts: 1, MaxExpandedBytes: 100},
		},
		"expanded bytes": {
			data:   append(newcEntry("large", []byte("12345"), 0o100644), newcEntry("TRAILER!!!", nil, 0)...),
			limits: limits{MaxArtifacts: 10, MaxExpandedBytes: 4},
		},
		"symlink target expansion": {
			data:   append(newcEntry("link", []byte("12345"), 0o120777), newcEntry("TRAILER!!!", nil, 0)...),
			limits: limits{MaxArtifacts: 10, MaxExpandedBytes: 4},
		},
		"truncated": {
			data: []byte("070701short"), limits: limits{MaxArtifacts: 10, MaxExpandedBytes: 100},
		},
		"crc format": {
			data: crcArchive, limits: limits{MaxArtifacts: 10, MaxExpandedBytes: 100},
			want: "070702 is not supported",
		},
		"oversized name": {
			data: oversizedName, limits: limits{MaxArtifacts: 10, MaxExpandedBytes: 100},
			want: "name size",
		},
		"truncated alignment padding": {
			data: truncatedPadding, limits: limits{MaxArtifacts: 10, MaxExpandedBytes: 100},
		},
		"non-empty trailer": {
			data:   newcEntry("TRAILER!!!", []byte("x"), 0),
			limits: limits{MaxArtifacts: 10, MaxExpandedBytes: 100},
			want:   "trailer has non-zero size",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseCPIO(tc.data, tc.limits)
			if err == nil {
				t.Fatal("accepted unsafe or malformed CPIO")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func newcEntry(name string, data []byte, mode uint32) []byte {
	return newcEntryMeta(name, data, mode, 1, 1, 0, 0)
}

func newcEntryMeta(name string, data []byte, mode, ino, nlink, major, minor uint32) []byte {
	return newcEntryMetaDevice(name, data, mode, ino, nlink, major, minor, 0, 0)
}

func newcEntryDevice(name string, mode, rmajor, rminor uint32) []byte {
	return newcEntryMetaDevice(name, nil, mode, 1, 1, 0, 0, rmajor, rminor)
}

func newcEntryMetaDevice(name string, data []byte, mode, ino, nlink, major, minor, rmajor, rminor uint32) []byte {
	nameBytes := append([]byte(name), 0)
	header := fmt.Sprintf(
		"070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		ino, mode, 0, 0, nlink, 0, len(data), major, minor, rmajor, rminor, len(nameBytes), 0,
	)
	out := append([]byte(header), nameBytes...)
	out = append(out, make([]byte, align4Test(len(out))-len(out))...)
	out = append(out, data...)
	out = append(out, make([]byte, align4Test(len(out))-len(out))...)
	return out
}

func align4Test(value int) int {
	return (value + 3) &^ 3
}
