package fwextract

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

func TestParseUBNTPartsAndEMMCRecords(t *testing.T) {
	part := ubntPart("kernel", []byte("legacy"))
	emmc := ubntEMMC("kernel0", []byte("modern"))
	image := ubntImage(append(part, emmc...))

	children, err := parseUBNT(image, defaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2", len(children))
	}
	if children[0].Name != "kernel" || string(children[0].Data) != "legacy" ||
		children[0].Offset != 268+56 {
		t.Fatalf("PART = %+v", children[0])
	}
	if children[1].Name != "kernel0" || string(children[1].Data) != "modern" ||
		children[1].Offset != int64(268+len(part)+56) {
		t.Fatalf("EMMC = %+v", children[1])
	}
}

func TestParseUBNTRejectsTruncatedPart(t *testing.T) {
	image := ubntImage(ubntPart("kernel", []byte("payload")))
	binary.BigEndian.PutUint32(image[268+48:268+52], 0xffffffff)
	if _, err := parseUBNT(image, defaultLimits()); err == nil {
		t.Fatal("accepted out-of-bounds PART")
	}
}

func TestParseUBNTRejectsTruncatedEndRecord(t *testing.T) {
	image := ubntImage(nil)
	image = image[:len(image)-1]
	if _, err := parseUBNT(image, defaultLimits()); err == nil {
		t.Fatal("accepted truncated ENDS record")
	}
}

func TestParseUBNTExecAndLegacyEndRecord(t *testing.T) {
	record := ubntExec("script", []byte("compressed script"))
	image := ubntLegacyImage(record)

	children, err := parseUBNT(image, defaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Name != "script" ||
		string(children[0].Data) != "compressed script" ||
		children[0].Offset != 268+56 {
		t.Fatalf("EXEC child = %+v", children)
	}
}

func TestDecodeDoesNotTreatUBNTExecScriptAsRootFilesystem(t *testing.T) {
	var archive bytes.Buffer
	tarWriter := tar.NewWriter(&archive)
	content := []byte("#!/bin/sh\n")
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: "bin/preflash", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	result := decode(ubntLegacyImage(ubntExec("script", compressed.Bytes())),
		"firmware.bin", defaultLimits())
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures = %+v", result.Failures)
	}
	if len(result.Roots) != 0 {
		t.Fatalf("decoded EXEC roots = %+v", result.Roots)
	}
}

func TestParseUBNTFileRecord(t *testing.T) {
	record := ubntFile("rootfs", []byte("filesystem"), 0x4000)
	image := ubntLegacyImage(record)

	children, err := parseUBNT(image, defaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Name != "rootfs" ||
		string(children[0].Data) != "filesystem" ||
		children[0].Offset != 268+56 {
		t.Fatalf("FILE child = %+v", children)
	}
}

func TestParseUBNTFileRejectsBadCRC(t *testing.T) {
	record := ubntFile("rootfs", []byte("filesystem"), 0x4000)
	record[len(record)-8] ^= 0xff
	if _, err := parseUBNT(ubntLegacyImage(record), defaultLimits()); err == nil {
		t.Fatal("accepted FILE with bad CRC")
	}
}

func TestDecodeTraversesUBNTFilePayload(t *testing.T) {
	archive := testTarArchive(t, map[string][]byte{"agent": []byte("vendor")})
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	result := decode(ubntLegacyImage(ubntFile("rootfs", compressed.Bytes(), 0x4000)),
		"firmware.bin", defaultLimits())
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures = %+v", result.Failures)
	}
	if !hasRootEntry(result, "firmware.bin/rootfs", "agent") {
		t.Fatalf("decoded FILE root = %+v", result.Roots)
	}
}

func TestDecodeIgnoresNestedInvalidUBNTHeaderCandidate(t *testing.T) {
	preloader := ubntLegacyImage(nil)
	preloader[260] ^= 0xff
	var archive bytes.Buffer
	tarWriter := tar.NewWriter(&archive)
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: "lib/ubnt/preloader.bin", Typeflag: tar.TypeReg,
		Mode: 0o644, Size: int64(len(preloader)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(preloader); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}

	result := decode(archive.Bytes(), "rootfs.tar", defaultLimits())
	if len(result.Failures) != 0 {
		t.Fatalf("nested invalid UBNT candidate caused failures: %+v", result.Failures)
	}
	if !hasRootEntry(result, "rootfs.tar", "lib/ubnt/preloader.bin") {
		t.Fatalf("nested root = %+v", result.Roots)
	}
}

func TestDecodeReportsTopLevelInvalidUBNTHeader(t *testing.T) {
	image := ubntLegacyImage(nil)
	image[260] ^= 0xff

	result := decode(image, "firmware.bin", defaultLimits())
	if len(result.Failures) != 1 || result.Failures[0].Stage != "ubnt" {
		t.Fatalf("top-level failures = %+v", result.Failures)
	}
}

func TestParseUBNTExecRejectsMismatchedSizes(t *testing.T) {
	record := ubntExec("script", []byte("payload"))
	binary.BigEndian.PutUint32(record[52:56], uint32(len("payload")+1))
	image := ubntLegacyImage(record)
	if _, err := parseUBNT(image, defaultLimits()); err == nil {
		t.Fatal("accepted EXEC with mismatched size fields")
	}
}

func TestParseUBNTLegacyEndRejectsBadCRC(t *testing.T) {
	image := ubntLegacyImage(nil)
	image[len(image)-8] ^= 0xff
	if _, err := parseUBNT(image, defaultLimits()); err == nil {
		t.Fatal("accepted legacy END. with bad CRC")
	}
}

func TestParseUBNTLegacyEndRejectsNonzeroReservedField(t *testing.T) {
	image := ubntLegacyImage(nil)
	image[len(image)-1] = 1
	if _, err := parseUBNT(image, defaultLimits()); err == nil {
		t.Fatal("accepted legacy END. with nonzero reserved field")
	}
}

func ubntImage(records []byte) []byte {
	header := make([]byte, 268)
	copy(header, "UBNTtest")
	binary.BigEndian.PutUint32(header[260:264], crc32.ChecksumIEEE(header[:260]))
	out := append(header, records...)
	out = append(out, []byte("ENDS")...)
	return append(out, make([]byte, 260)...)
}

func ubntLegacyImage(records []byte) []byte {
	header := make([]byte, 268)
	copy(header, "UBNTtest")
	binary.BigEndian.PutUint32(header[260:264], crc32.ChecksumIEEE(header[:260]))
	out := append(header, records...)
	end := make([]byte, 12)
	copy(end, "END.")
	binary.BigEndian.PutUint32(end[4:8], crc32.ChecksumIEEE(out))
	return append(out, end...)
}

func ubntPart(name string, data []byte) []byte {
	header := make([]byte, 56)
	copy(header, "PART")
	copy(header[4:20], name)
	binary.BigEndian.PutUint32(header[48:52], uint32(len(data)))
	binary.BigEndian.PutUint32(header[52:56], uint32(len(data)))
	out := append(header, data...)
	trailer := make([]byte, 8)
	binary.BigEndian.PutUint32(trailer[:4], crc32.ChecksumIEEE(out))
	return append(out, trailer...)
}

func ubntEMMC(name string, data []byte) []byte {
	header := make([]byte, 56)
	copy(header, "EMMC")
	copy(header[4:36], name)
	binary.BigEndian.PutUint32(header[48:52], uint32(len(data)))
	out := append(header, data...)
	trailer := make([]byte, 8)
	binary.BigEndian.PutUint32(trailer[:4], crc32.ChecksumIEEE(out))
	return append(out, trailer...)
}

func ubntExec(name string, data []byte) []byte {
	header := make([]byte, 56)
	copy(header, "EXEC")
	copy(header[4:36], name)
	binary.BigEndian.PutUint32(header[36:40], 1)
	binary.BigEndian.PutUint32(header[48:52], uint32(len(data)))
	binary.BigEndian.PutUint32(header[52:56], uint32(len(data)))
	out := append(header, data...)
	trailer := make([]byte, 8)
	binary.BigEndian.PutUint32(trailer[:4], crc32.ChecksumIEEE(out))
	return append(out, trailer...)
}

func ubntFile(name string, data []byte, alignment uint32) []byte {
	header := make([]byte, 56)
	copy(header, "FILE")
	copy(header[4:36], name)
	binary.BigEndian.PutUint32(header[48:52], uint32(len(data)))
	binary.BigEndian.PutUint32(header[52:56], alignment)
	out := append(header, data...)
	trailer := make([]byte, 8)
	binary.BigEndian.PutUint32(trailer[:4], crc32.ChecksumIEEE(out))
	return append(out, trailer...)
}
