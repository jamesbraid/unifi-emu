package fwextract

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/KarpelesLab/squashfs"
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

	result := decode(image, "firmware.bin", limits{MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 10})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if !hasRootEntry(result, "firmware.bin/kernel/kernel", "usr/bin/mcad") {
		t.Fatalf("mcad root not extracted: %+v", result.Roots)
	}
}

func TestDecodeHandlesGzipTar(t *testing.T) {
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

	result := decode(gz.Bytes(), "gzip", limits{MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 10})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if !hasRootEntry(result, "gzip", "usr/bin/mcad") {
		t.Fatalf("tar root not extracted: %+v", result.Roots)
	}
}

func TestDecodeStopsAtRecognizedRootFilesystem(t *testing.T) {
	nested := append(newcEntry("usr/bin/mcad", []byte("nested"), 0o100755),
		newcEntry("TRAILER!!!", nil, 0)...)
	var root []byte
	root = append(root, newcEntry("usr/bin/mcad", []byte("root"), 0o100755)...)
	root = append(root, newcEntry("opt/nested.cpio", nested, 0o100644)...)
	root = append(root, newcEntry("TRAILER!!!", nil, 0)...)

	result := decode(root, "firmware.bin/initramfs", limits{
		MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 10,
	})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if len(result.Roots) != 1 ||
		result.Roots[0].Artifact != "firmware.bin/initramfs" {
		t.Fatalf("decoded roots = %+v", result.Roots)
	}
}

func TestDecodeRecursesThroughNonRootArchiveToNestedRoot(t *testing.T) {
	nested := append(newcEntry("usr/bin/mcad", []byte("nested"), 0o100755),
		newcEntry("TRAILER!!!", nil, 0)...)
	outer := testTarArchive(t, map[string][]byte{"payload.cpio": nested})

	result := decode(outer, "firmware.bin/package.tar", limits{
		MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 10,
	})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if len(result.Roots) != 1 ||
		result.Roots[0].Artifact != "firmware.bin/package.tar/payload.cpio" {
		t.Fatalf("decoded roots = %+v", result.Roots)
	}
}

func TestDecodeMergesMicrocodeFirstConcatenatedInitramfs(t *testing.T) {
	microcode := append(
		newcEntry("kernel/x86/microcode/GenuineIntel.bin", []byte("microcode"), 0o100644),
		newcEntry("TRAILER!!!", nil, 0)...,
	)
	padding := make([]byte, 512-len(microcode)%512)
	root := append(newcEntry("usr/bin/mcad", []byte("agent"), 0o100755),
		newcEntry("TRAILER!!!", nil, 0)...)
	initramfs := append(append(microcode, padding...), root...)

	result := decode(initramfs, "firmware.bin/initramfs", limits{
		MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 10,
	})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if len(result.Roots) != 1 ||
		!hasRootEntry(result, "firmware.bin/initramfs", "kernel/x86/microcode/GenuineIntel.bin") ||
		!hasRootEntry(result, "firmware.bin/initramfs", "usr/bin/mcad") {
		t.Fatalf("concatenated initramfs root = %+v", result.Roots)
	}
}

func TestDecodeCarvesConcatenatedCPIOBeforeKernelTail(t *testing.T) {
	microcode := append(
		newcEntry("kernel/x86/microcode/GenuineIntel.bin", []byte("microcode"), 0o100644),
		newcEntry("TRAILER!!!", nil, 0)...,
	)
	padding := make([]byte, 512-len(microcode)%512)
	root := append(newcEntry("usr/bin/mcad", []byte("agent"), 0o100755),
		newcEntry("TRAILER!!!", nil, 0)...)
	initramfs := append(append(microcode, padding...), root...)
	kernel := append([]byte("kernel-prefix"), initramfs...)
	kernel = append(kernel, []byte("non-cpio-kernel-tail")...)

	result := decode(kernel, "firmware.bin/kernel", limits{
		MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 10,
	})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if len(result.Roots) != 1 ||
		!hasRootEntry(result, "firmware.bin/kernel/initramfs", "kernel/x86/microcode/GenuineIntel.bin") ||
		!hasRootEntry(result, "firmware.bin/kernel/initramfs", "usr/bin/mcad") {
		t.Fatalf("embedded concatenated initramfs root = %+v", result.Roots)
	}
}

func TestDecodeRootFSNamedTarDescendsIntoSquashFS(t *testing.T) {
	var image bytes.Buffer
	writer, err := squashfs.NewWriter(&image)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AddDirectory("usr", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddDirectory("usr/bin", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddFile("usr/bin/mcad", []byte("agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writer.Finalize(); err != nil {
		t.Fatal(err)
	}
	outer := testTarArchive(t, map[string][]byte{"payload.squashfs": image.Bytes()})

	result := decode(outer, "firmware.bin/rootfs.tar", limits{
		MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 10,
	})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if len(result.Roots) != 1 ||
		result.Roots[0].Artifact != "firmware.bin/rootfs.tar/payload.squashfs" {
		t.Fatalf("decoded roots = %+v", result.Roots)
	}
}

func TestDecodePrefixedMcadDoesNotMakePackageRoot(t *testing.T) {
	outer := testTarArchive(t, map[string][]byte{
		"payload/usr/bin/mcad": []byte("agent"),
	})

	result := decode(outer, "firmware.bin/package.tar", limits{
		MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 10,
	})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if len(result.Roots) != 0 {
		t.Fatalf("packaging wrapper became root = %+v", result.Roots)
	}
}

func TestFITLZOCompressionUsesLZOPFraming(t *testing.T) {
	state := decoderState{limits: limits{MaxExpandedBytes: 1 << 20}.withDefaults()}
	_, err := state.decompress([]byte("not an lzop stream"), "lzo")
	if err == nil || !strings.Contains(err.Error(), "not an lzop stream") {
		t.Fatalf("FIT LZO framing error = %v", err)
	}
}

func TestDecompressLZOPRequiresFramedStream(t *testing.T) {
	_, err := decompressLZOP([]byte("not an lzop stream"), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "not an lzop stream") {
		t.Fatalf("lzop framing error = %v", err)
	}
}

func TestDecompressLZOPReportsRuntimePrerequisite(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	input := append(append([]byte(nil), lzopMagic...), byte(0))
	_, err := decompressLZOP(input, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "lzop executable is required") {
		t.Fatalf("prerequisite error = %v", err)
	}
}

func TestDecompressLZOPWithCommandPipesFramedData(t *testing.T) {
	input := append(append([]byte(nil), lzopMagic...), []byte("framed payload")...)
	decoded, err := decompressLZOPWithCommand(input, 1<<20,
		func(stdin io.Reader, stdout, _ io.Writer) error {
			got, err := io.ReadAll(stdin)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, input) {
				return fmt.Errorf("stdin = %x, want %x", got, input)
			}
			_, err = stdout.Write([]byte("agent"))
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "agent" {
		t.Fatalf("decoded = %q", decoded)
	}
}

func TestDecompressLZOPWithCommandEnforcesOutputLimit(t *testing.T) {
	input := append(append([]byte(nil), lzopMagic...), byte(0))
	_, err := decompressLZOPWithCommand(input, 4,
		func(_ io.Reader, stdout, _ io.Writer) error {
			_, err := stdout.Write([]byte("12345"))
			return err
		})
	if err == nil || !strings.Contains(err.Error(), "exceed limit 4") {
		t.Fatalf("limit error = %v", err)
	}
}

func TestDecompressLZOPWithCommandReportsExitError(t *testing.T) {
	input := append(append([]byte(nil), lzopMagic...), byte(0))
	_, err := decompressLZOPWithCommand(input, 1<<20,
		func(_ io.Reader, _, stderr io.Writer) error {
			_, _ = stderr.Write([]byte("bad frame"))
			return errors.New("exit status 1")
		})
	if err == nil || !strings.Contains(err.Error(), "exit status 1") ||
		!strings.Contains(err.Error(), "bad frame") {
		t.Fatalf("command error = %v", err)
	}
}

func TestCloseDecompressorPreservesPrimaryAndCloseErrors(t *testing.T) {
	primary := errors.New("read failure")
	closeErr := errors.New("close failure")
	err := closeDecompressor(func() error { return closeErr }, primary)
	if !errors.Is(err, primary) || !errors.Is(err, closeErr) {
		t.Fatalf("joined error = %v", err)
	}
}

func TestDecodeReportsDepthAndExpansionLimits(t *testing.T) {
	nested := append(newcEntry("file", []byte("12345"), 0o100644), newcEntry("TRAILER!!!", nil, 0)...)
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(nested)
	_ = gw.Close()
	result := decode(gz.Bytes(), "limited", limits{MaxArtifacts: 10, MaxExpandedBytes: 4, MaxDepth: 1})
	if len(result.Failures) == 0 {
		t.Fatal("limit violation was not reported")
	}
}

func TestDecodeAppliesExpansionLimitAcrossNestedArchives(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 256)
	inner := testTarArchive(t, map[string][]byte{"payload": payload})
	outer := testTarArchive(t, map[string][]byte{"inner.tar": inner})
	limit := int64(len(inner) + len(payload) - 1)

	result := decode(outer, "nested.tar", limits{
		MaxArtifacts: 10, MaxExpandedBytes: limit, MaxDepth: 4,
	})
	if !hasExpandedFailure(result) {
		t.Fatalf("nested archives bypassed global expanded-byte limit %d: %+v", limit, result.Failures)
	}
	exact := decode(outer, "nested.tar", limits{
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

	result := decode(outer, "siblings.tar", limits{
		MaxArtifacts: 20, MaxExpandedBytes: limit, MaxDepth: 4,
	})
	if !hasExpandedFailure(result) {
		t.Fatalf("sibling archives bypassed global expanded-byte limit %d: %+v", limit, result.Failures)
	}
}

func TestDecodeCarvesEmbeddedCPIOFromKernel(t *testing.T) {
	cpio := append(newcEntry("usr/bin/mcad", []byte("fw_caps\x00"), 0o100755), newcEntry("TRAILER!!!", nil, 0)...)
	kernel := append(bytes.Repeat([]byte{0xaa}, 37), cpio...)
	result := decode(kernel, "vmlinux", limits{MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 4})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if !hasRootEntry(result, "vmlinux/initramfs", "usr/bin/mcad") {
		t.Fatalf("embedded root = %+v", result.Roots)
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

	result := decode(kernel, "vmlinux", limits{MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 5})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if !hasRootEntry(result, "vmlinux/embedded.xz", "usr/bin/mcad") {
		t.Fatalf("embedded XZ root = %+v", result.Roots)
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

	result := decode(kernel, "vmlinux", limits{MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 5})
	if len(result.Failures) != 0 {
		t.Fatalf("decode failures: %+v", result.Failures)
	}
	if !hasRootEntry(result, "vmlinux/embedded.lzma", "usr/bin/mcad") {
		t.Fatalf("embedded LZMA root = %+v", result.Roots)
	}
}

func TestDecodeEmbeddedLZMASkipsLaterFalseCandidate(t *testing.T) {
	cpio := append(newcEntry("usr/bin/mcad", []byte("fw_caps\x00"), 0o100755),
		newcEntry("TRAILER!!!", nil, 0)...)
	valid := testLZMAStream(t, cpio)
	kernel := append(bytes.Repeat([]byte{0xaa}, 19), valid...)
	kernel = append(kernel, bytes.Repeat([]byte{0xbb}, 23)...)
	kernel = append(kernel, falseLZMACandidate(t)...)

	result := decode(kernel, "vmlinux", limits{
		MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 5,
	})
	if len(result.Failures) != 0 {
		t.Fatalf("later false candidate caused failures: %+v", result.Failures)
	}
	if !hasRootEntry(result, "vmlinux/embedded.lzma", "usr/bin/mcad") {
		t.Fatalf("earlier valid LZMA root = %+v", result.Roots)
	}
}

func TestDecodeEmbeddedLZMAIgnoresIsolatedFalseCandidate(t *testing.T) {
	kernel := append(bytes.Repeat([]byte{0xaa}, 19), falseLZMACandidate(t)...)

	result := decode(kernel, "vmlinux", limits{
		MaxArtifacts: 20, MaxExpandedBytes: 1 << 20, MaxDepth: 5,
	})
	if len(result.Failures) != 0 {
		t.Fatalf("isolated false candidate caused failures: %+v", result.Failures)
	}
	if len(result.Roots) != 0 {
		t.Fatalf("false candidate produced root = %+v", result.Roots)
	}
}

func hasRootEntry(result decodeResult, artifact, path string) bool {
	for _, root := range result.Roots {
		if root.Artifact != artifact {
			continue
		}
		for _, entry := range root.Entries {
			if entry.Path == path {
				return true
			}
		}
	}
	return false
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

func hasExpandedFailure(result decodeResult) bool {
	for _, failure := range result.Failures {
		if strings.Contains(failure.Error, "expanded bytes") {
			return true
		}
	}
	return false
}
