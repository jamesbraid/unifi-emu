package fwextract

import (
	"bytes"
	"io"
	"io/fs"
	"testing"

	"github.com/KarpelesLab/squashfs"
	"github.com/pierrec/lz4/v4"
)

func TestParseSquashFSRejectsTruncatedImageWithoutPanicking(t *testing.T) {
	data := []byte{'h', 's', 'q', 's'}
	if _, err := parseSquashFS(data, defaultLimits()); err == nil {
		t.Fatal("accepted truncated SquashFS image")
	}
}

func TestSquashFSWalkerReadsDirectoriesInBoundedChunks(t *testing.T) {
	directory := &recordingReadDirFile{}
	walker := squashFSWalker{limits: limits{MaxArtifacts: 10}.withDefaults()}

	if err := walker.walkDirectory(directory, ""); err != nil {
		t.Fatal(err)
	}
	if len(directory.readSizes) != 1 || directory.readSizes[0] != 11 {
		t.Fatalf("ReadDir calls = %v, want remaining budget plus one", directory.readSizes)
	}
}

func TestParseSquashFSReadsEmptyFile(t *testing.T) {
	var image bytes.Buffer
	writer, err := squashfs.NewWriter(&image)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AddFile("empty", nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writer.Finalize(); err != nil {
		t.Fatal(err)
	}

	entries, err := parseSquashFS(image.Bytes(), limits{
		MaxArtifacts: 10, MaxExpandedBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "empty" ||
		entries[0].Kind != "regular" || entries[0].Mode != 0o640 ||
		len(entries[0].Data) != 0 {
		t.Fatalf("empty file = %+v", entries)
	}
}

func TestParseSquashFSReadsRawLZ4Blocks(t *testing.T) {
	squashfs.RegisterCompHandler(squashfs.LZ4, &squashfs.CompHandler{
		Decompress: decompressSquashFSLZ4,
		Compress:   compressSquashFSLZ4ForTest,
	})
	var image bytes.Buffer
	writer, err := squashfs.NewWriter(&image,
		squashfs.WithCompression(squashfs.LZ4),
		squashfs.WithBlockSize(4096))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AddDirectory("usr", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddDirectory("usr/bin", 0o755); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("raw-lz4-block\n"), 1024)
	if err := writer.AddFile("usr/bin/mcad", content, fs.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	if err := writer.Finalize(); err != nil {
		t.Fatal(err)
	}

	entries, err := parseSquashFS(image.Bytes(), limits{
		MaxArtifacts: 10, MaxExpandedBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %+v", entries)
	}
	mcad := entries[2]
	if mcad.Path != "usr/bin/mcad" || mcad.Kind != "regular" ||
		mcad.Mode != 0o4755 || !bytes.Equal(mcad.Data, content) {
		t.Fatalf("mcad = %+v", mcad)
	}
}

func TestParseSquashFSPreservesSymlinkMetadata(t *testing.T) {
	content := []byte("same inode")
	var image bytes.Buffer
	writer, err := squashfs.NewWriter(&image)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AddFile("a", content, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddSymlink("link", "a"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Finalize(); err != nil {
		t.Fatal(err)
	}

	entries, err := parseSquashFS(image.Bytes(), limits{
		MaxArtifacts: 10, MaxExpandedBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Mode != 0o640 || entries[0].Kind != "regular" {
		t.Fatalf("regular = %+v", entries[0])
	}
	if entries[1].Kind != "symlink" || entries[1].Linkname != "a" ||
		entries[1].Mode != 0o777 {
		t.Fatalf("symlink = %+v", entries[1])
	}
}

func TestParseSquashFSPreservesDeviceNodesAndFIFO(t *testing.T) {
	var image bytes.Buffer
	writer, err := squashfs.NewWriter(&image)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AddDevice("null", fs.ModeCharDevice|0o666, 1<<8|3); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddDevice("mtdblock0", fs.ModeDevice|0o640, 31<<8); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddFifo("events", 0o620); err != nil {
		t.Fatal(err)
	}
	if err := writer.Finalize(); err != nil {
		t.Fatal(err)
	}

	entries, err := parseSquashFS(image.Bytes(), limits{
		MaxArtifacts: 10, MaxExpandedBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %+v", entries)
	}
	byPath := make(map[string]decodedFile, len(entries))
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	if got := byPath["null"]; got.Kind != "char-device" ||
		got.DeviceMajor != 1 || got.DeviceMinor != 3 || got.Mode != 0o666 {
		t.Fatalf("character device = %+v", got)
	}
	if got := byPath["mtdblock0"]; got.Kind != "block-device" ||
		got.DeviceMajor != 31 || got.DeviceMinor != 0 || got.Mode != 0o640 {
		t.Fatalf("block device = %+v", got)
	}
	if got := byPath["events"]; got.Kind != "fifo" || got.Mode != 0o620 {
		t.Fatalf("FIFO = %+v", got)
	}
}

func TestParseSquashFSRejectsSocketInsteadOfSkippingIt(t *testing.T) {
	var image bytes.Buffer
	writer, err := squashfs.NewWriter(&image)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AddSocket("control.sock", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writer.Finalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := parseSquashFS(image.Bytes(), defaultLimits()); err == nil {
		t.Fatal("SquashFS socket was silently skipped")
	}
}

func TestSquashFSLinkKeyPreservesHardlinkInodeIdentity(t *testing.T) {
	first := &squashfs.Inode{Ino: 42, NLink: 2}
	second := &squashfs.Inode{Ino: 42, NLink: 2}
	if got, want := squashFSLinkKey(first), squashFSLinkKey(second); got == "" || got != want {
		t.Fatalf("hardlink keys = %q, %q", got, want)
	}
	if got := squashFSLinkKey(&squashfs.Inode{Ino: 42, NLink: 1}); got != "" {
		t.Fatalf("single-link key = %q", got)
	}
}

func compressSquashFSLZ4ForTest(src []byte) ([]byte, error) {
	dst := make([]byte, lz4.CompressBlockBound(len(src)))
	n, err := lz4.CompressBlock(src, dst, nil)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	return dst[:n], nil
}

type recordingReadDirFile struct {
	readSizes []int
}

func (f *recordingReadDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	f.readSizes = append(f.readSizes, n)
	return nil, io.EOF
}

func (*recordingReadDirFile) Stat() (fs.FileInfo, error) { return nil, nil }
func (*recordingReadDirFile) Read([]byte) (int, error)   { return 0, io.EOF }
func (*recordingReadDirFile) Close() error               { return nil }
