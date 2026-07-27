package firmware

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"hash/adler32"
	"io"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

func TestDecodeRecursesThroughUBNTUImageLZMAAndCPIO(t *testing.T) {
	cpio := append(newcEntry("usr/bin/mcad", []byte("fw_caps\x00setparam\x00"), 0o100755), newcEntry("TRAILER!!!", nil, 0)...)
	var compressed bytes.Buffer
	writer, err := lzma.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(cpio); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	image := ubntImage(ubntPart("kernel", testUImage("kernel", 3, compressed.Bytes())))

	result := Decode(image, "firmware.bin", Limits{MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 10})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	found := false
	for _, file := range result.Files {
		if file.Path == "firmware.bin/kernel/kernel/usr/bin/mcad" &&
			bytes.Contains(file.Data, []byte("fw_caps")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("mcad not extracted: %+v", result.Artifacts)
	}
}

func TestDecodeHandlesGzipTarAndLZOP(t *testing.T) {
	var tarData bytes.Buffer
	tw := tar.NewWriter(&tarData)
	if err := tw.WriteHeader(&tar.Header{Name: "usr/bin/mcad", Mode: 0o755, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("agent")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(tarData.Bytes())
	_ = gw.Close()

	for name, input := range map[string][]byte{
		"gzip": gz.Bytes(),
		"lzop": testLZOP([]byte("agent")),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "lzop" {
				if _, err := exec.LookPath("lzop"); err != nil {
					t.Skip("lzop executable is not installed")
				}
			}
			result := Decode(input, name, Limits{MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 10})
			if len(result.Failures) != 0 {
				t.Fatalf("decode failures: %+v", result.Failures)
			}
			if len(result.Files) == 0 {
				t.Fatalf("no decoded files: %+v", result.Artifacts)
			}
		})
	}
}

func TestDecodeHandlesBzip2LZ4AndZstandard(t *testing.T) {
	payload := []byte("agent")
	bzip, err := hex.DecodeString("425a6839314159265359657a6bce00000001802281040020002186819a0c537177245385090657a6bce0")
	if err != nil {
		t.Fatal(err)
	}
	var lz4Data bytes.Buffer
	lz4Writer := lz4.NewWriter(&lz4Data)
	_, _ = lz4Writer.Write(payload)
	_ = lz4Writer.Close()
	var zstdData bytes.Buffer
	zstdWriter, err := zstd.NewWriter(&zstdData)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = zstdWriter.Write(payload)
	zstdWriter.Close()

	for name, input := range map[string][]byte{
		"bzip2": bzip,
		"lz4":   lz4Data.Bytes(),
		"zstd":  zstdData.Bytes(),
	} {
		t.Run(name, func(t *testing.T) {
			result := Decode(input, name, Limits{MaxArtifacts: 10, MaxExpandedBytes: 1 << 20, MaxDepth: 4})
			if len(result.Failures) != 0 {
				t.Fatalf("decode failures: %+v", result.Failures)
			}
			if len(result.Files) != 1 || string(result.Files[0].Data) != "agent" {
				t.Fatalf("decoded files = %+v", result.Files)
			}
		})
	}
}

func TestDecodeReportsDepthAndExpansionLimits(t *testing.T) {
	var nested []byte = append(newcEntry("file", []byte("12345"), 0o100644), newcEntry("TRAILER!!!", nil, 0)...)
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(nested)
	_ = gw.Close()
	result := Decode(gz.Bytes(), "limited", Limits{MaxArtifacts: 10, MaxExpandedBytes: 4, MaxDepth: 1})
	if len(result.Failures) == 0 {
		t.Fatal("limit violation was not reported")
	}
}

func TestDecodeAppliesExpansionLimitAcrossNestedArchives(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 256)
	inner := testTarArchive(t, map[string][]byte{"payload": payload})
	outer := testTarArchive(t, map[string][]byte{"inner.tar": inner})
	limit := int64(len(inner) + len(payload) - 1)

	result := Decode(outer, "nested.tar", Limits{
		MaxArtifacts: 10, MaxExpandedBytes: limit, MaxDepth: 4,
	})
	if !hasExpandedFailure(result) {
		t.Fatalf("nested archives bypassed global expanded-byte limit %d: %+v", limit, result.Failures)
	}
	exact := Decode(outer, "nested.tar", Limits{
		MaxArtifacts: 10, MaxExpandedBytes: limit + 1, MaxDepth: 4,
	})
	if len(exact.Failures) != 0 {
		t.Fatalf("exact aggregate expanded-byte budget failed: %+v", exact.Failures)
	}
}

func TestDecodeAppliesExpansionLimitAcrossSiblingArchives(t *testing.T) {
	payloadA := bytes.Repeat([]byte("a"), 256)
	payloadB := bytes.Repeat([]byte("b"), 256)
	innerA := testTarArchive(t, map[string][]byte{"a": payloadA})
	innerB := testTarArchive(t, map[string][]byte{"b": payloadB})
	outer := testTarArchive(t, map[string][]byte{
		"a.tar": innerA,
		"b.tar": innerB,
	})
	limit := int64(len(innerA) + len(innerB) + len(payloadA) + len(payloadB) - 1)

	result := Decode(outer, "siblings.tar", Limits{
		MaxArtifacts: 20, MaxExpandedBytes: limit, MaxDepth: 4,
	})
	if !hasExpandedFailure(result) {
		t.Fatalf("sibling archives bypassed global expanded-byte limit %d: %+v", limit, result.Failures)
	}
}

func TestDecodeCarvesEmbeddedCPIOFromKernel(t *testing.T) {
	cpio := append(newcEntry("usr/bin/mcad", []byte("fw_caps\x00"), 0o100755), newcEntry("TRAILER!!!", nil, 0)...)
	kernel := append(bytes.Repeat([]byte{0xaa}, 37), cpio...)
	result := Decode(kernel, "vmlinux", Limits{MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 4})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "vmlinux/initramfs/usr/bin/mcad" ||
		result.Files[0].Offset != int64(align4(newcHeaderSize+len("usr/bin/mcad")+1)) {
		t.Fatalf("embedded files = %+v", result.Files)
	}
}

func TestDecodeCarvesEmbeddedXZFromKernel(t *testing.T) {
	cpio := append(newcEntry("usr/bin/mcad", []byte("fw_caps\x00"), 0o100755), newcEntry("TRAILER!!!", nil, 0)...)
	var compressed bytes.Buffer
	writer, err := xz.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write(cpio)
	_ = writer.Close()
	kernel := append(bytes.Repeat([]byte{0xaa}, 23), xzMagic...)
	kernel = append(kernel, []byte("not an xz header")...)
	corrupt := append([]byte(nil), compressed.Bytes()...)
	corrupt[len(corrupt)-1] ^= 0xff
	kernel = append(kernel, corrupt...)
	kernel = append(kernel, compressed.Bytes()...)

	result := Decode(kernel, "vmlinux", Limits{MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 5})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "vmlinux/embedded.xz/usr/bin/mcad" {
		t.Fatalf("embedded XZ files = %+v", result.Files)
	}
}

func TestDecodeCarvesEmbeddedLZMAFromKernel(t *testing.T) {
	cpio := append(newcEntry("usr/bin/mcad", []byte("fw_caps\x00"), 0o100755), newcEntry("TRAILER!!!", nil, 0)...)
	var compressed bytes.Buffer
	writer, err := lzma.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write(cpio)
	_ = writer.Close()
	kernel := append(bytes.Repeat([]byte{0xaa}, 19), compressed.Bytes()...)

	result := Decode(kernel, "vmlinux", Limits{MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 5})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "vmlinux/embedded.lzma/usr/bin/mcad" {
		t.Fatalf("embedded LZMA files = %+v", result.Files)
	}
}

func TestDecodeEmbeddedLZMASkipsLaterFalseCandidate(t *testing.T) {
	cpio := append(newcEntry("usr/bin/mcad", []byte("fw_caps\x00"), 0o100755),
		newcEntry("TRAILER!!!", nil, 0)...)
	valid := testLZMAStream(t, cpio)
	kernel := append(bytes.Repeat([]byte{0xaa}, 19), valid...)
	kernel = append(kernel, bytes.Repeat([]byte{0xbb}, 23)...)
	kernel = append(kernel, falseLZMACandidate(t)...)

	result := Decode(kernel, "vmlinux", Limits{
		MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 5,
	})
	if len(result.Failures) != 0 {
		t.Fatalf("later false candidate caused failures: %+v", result.Failures)
	}
	if len(result.Files) != 1 ||
		result.Files[0].Path != "vmlinux/embedded.lzma/usr/bin/mcad" {
		t.Fatalf("earlier valid LZMA files = %+v", result.Files)
	}
}

func TestDecodeEmbeddedLZMAIgnoresIsolatedFalseCandidate(t *testing.T) {
	kernel := append(bytes.Repeat([]byte{0xaa}, 19), falseLZMACandidate(t)...)

	result := Decode(kernel, "vmlinux", Limits{
		MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 5,
	})
	if len(result.Failures) != 0 {
		t.Fatalf("isolated false candidate caused failures: %+v", result.Failures)
	}
	if len(result.Files) != 1 || result.Files[0].Path != "vmlinux" {
		t.Fatalf("opaque kernel files = %+v", result.Files)
	}
}

func testLZMAStream(t *testing.T, content []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer, err := lzma.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func falseLZMACandidate(t *testing.T) []byte {
	t.Helper()
	candidate, err := hex.DecodeString(
		"0000200000ffffffffffffffff0000000000000000003b00000086800000601900" +
			"00bfe40000ffffffff0000000000000000004f0000005d11000003010000ffffff",
	)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := lzma.NewReader(bytes.NewReader(candidate))
	if err != nil {
		t.Fatalf("false candidate header does not open: %v", err)
	}
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("false candidate unexpectedly decompresses")
	}
	return candidate
}

func testLZOP(data []byte) []byte {
	const version uint16 = 0x1040
	var header bytes.Buffer
	_ = binary.Write(&header, binary.BigEndian, version)
	_ = binary.Write(&header, binary.BigEndian, uint16(0x20a0))
	_ = binary.Write(&header, binary.BigEndian, uint16(0x0940))
	header.WriteByte(3)
	header.WriteByte(9)
	_ = binary.Write(&header, binary.BigEndian, uint32(1))
	_ = binary.Write(&header, binary.BigEndian, uint32(0o100644))
	_ = binary.Write(&header, binary.BigEndian, uint32(0))
	_ = binary.Write(&header, binary.BigEndian, uint32(0))
	header.WriteByte(0)

	out := append([]byte{0x89, 'L', 'Z', 'O', 0, '\r', '\n', 0x1a, '\n'}, header.Bytes()...)
	check := make([]byte, 4)
	binary.BigEndian.PutUint32(check, adler32.Checksum(header.Bytes()))
	out = append(out, check...)
	block := make([]byte, 12)
	binary.BigEndian.PutUint32(block[0:4], uint32(len(data)))
	binary.BigEndian.PutUint32(block[4:8], uint32(len(data)))
	binary.BigEndian.PutUint32(block[8:12], adler32.Checksum(data))
	out = append(out, block...)
	out = append(out, data...)
	return append(out, 0, 0, 0, 0)
}

func testTarArchive(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := entries[name]
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func hasExpandedFailure(result DecodeResult) bool {
	for _, failure := range result.Failures {
		if strings.Contains(failure.Error, "expanded bytes") {
			return true
		}
	}
	return false
}
