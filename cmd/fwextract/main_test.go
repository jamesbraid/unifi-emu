package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamesbraid/unifi-emu/fwextract"
	"github.com/u-root/u-root/pkg/cpio"
)

func TestRunWritesFixedExtractionContract(t *testing.T) {
	image := commandUBNTImage(commandRootFS(t))
	sum := fmt.Sprintf("%x", sha256.Sum256(image))
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "firmware.bin")
	outputDir := filepath.Join(dir, "output")
	if err := os.WriteFile(imagePath, image, 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	err := run([]string{
		"-image", imagePath,
		"-sha256", sum,
		"-platform", "TEST",
		"-version", "1.2.3",
		"-out", outputDir,
	}, &stderr)
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	tarPath := filepath.Join(outputDir, "rootfs.tar")
	tarFile, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tarFile.Close()
	reader := tar.NewReader(tarFile)
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "usr/bin/mcad" {
		t.Fatalf("first tar entry = %q", header.Name)
	}
	payload, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "vendor" {
		t.Fatalf("mcad payload = %q", payload)
	}

	meta, err := os.ReadFile(filepath.Join(outputDir, "rootfs.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"source_firmware_digest": "` + sum + `"`,
		`"platform": "TEST"`,
		`"version": "1.2.3"`,
		`"nested_artifact_path": "firmware.bin/agent"`,
	} {
		if !strings.Contains(string(meta), want) {
			t.Fatalf("rootfs.json missing %s:\n%s", want, meta)
		}
	}
}

func TestRunRequiresLocalImageMetadataAndOutputDirectory(t *testing.T) {
	validSHA := strings.Repeat("a", 64)
	for name, args := range map[string][]string{
		"image":    {"-sha256", validSHA, "-platform", "TEST", "-version", "1", "-out", "output"},
		"sha256":   {"-image", "firmware.bin", "-platform", "TEST", "-version", "1", "-out", "output"},
		"platform": {"-image", "firmware.bin", "-sha256", validSHA, "-version", "1", "-out", "output"},
		"version":  {"-image", "firmware.bin", "-sha256", validSHA, "-platform", "TEST", "-out", "output"},
		"output":   {"-image", "firmware.bin", "-sha256", validSHA, "-platform", "TEST", "-version", "1"},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(args, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("error = %v, want missing %s", err, name)
			}
		})
	}
}

func TestReadFirmwareRejectsSparseOversizedFileBeforeAllocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((1 << 30) + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = readFirmware(path)
	if err == nil || !strings.Contains(err.Error(), "exceeds limit 1073741824") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunRejectsAcquisitionAndAnalysisFlags(t *testing.T) {
	for _, flagName := range []string{
		"-index", "-index-url", "-cache", "-all", "-live", "-capability-bits",
		"-rootfs-tar", "-rootfs-json", "-max-artifacts", "-min-string-length",
	} {
		t.Run(flagName, func(t *testing.T) {
			err := run([]string{flagName, "value"}, &bytes.Buffer{})
			if err == nil {
				t.Fatalf("accepted removed flag %s", flagName)
			}
		})
	}
}

func TestPublishRemovesOldCommitMarkerBeforeReplacingTar(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output")
	if err := os.Mkdir(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(outputDir, "rootfs.tar")
	jsonPath := filepath.Join(outputDir, "rootfs.json")
	if err := os.WriteFile(tarPath, []byte("old tar"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldMetadata := []byte(`{"tar_digest":"old valid digest"}` + "\n")
	if err := os.WriteFile(jsonPath, oldMetadata, 0o644); err != nil {
		t.Fatal(err)
	}
	stagedTar, err := createOutputTemp(outputDir, ".rootfs-*.tar.tmp")
	if err != nil {
		t.Fatal(err)
	}
	stagedTarPath := stagedTar.Name()
	if _, err := stagedTar.Write([]byte("new tar")); err != nil {
		t.Fatal(err)
	}
	if err := stagedTar.Close(); err != nil {
		t.Fatal(err)
	}
	metadata := fwextract.Metadata{
		SourceFirmwareDigest: strings.Repeat("a", 64),
		Platform:             "TEST",
		Version:              "1",
		NestedArtifactPath:   "firmware.bin/rootfs",
		TarDigest:            strings.Repeat("b", 64),
		EntryCount:           1,
	}
	renames := 0
	err = publishWithRename(outputDir, stagedTarPath, metadata, func(oldPath, newPath string) error {
		renames++
		if renames == 2 {
			return fmt.Errorf("injected metadata failure")
		}
		return os.Rename(oldPath, newPath)
	})
	if err == nil || !strings.Contains(err.Error(), "publish rootfs metadata") {
		t.Fatalf("error = %v", err)
	}
	if got, err := os.ReadFile(tarPath); err != nil || string(got) != "new tar" {
		t.Fatalf("rootfs.tar = %q, %v", got, err)
	}
	if _, err := os.Stat(jsonPath); !os.IsNotExist(err) {
		t.Fatalf("stale rootfs.json survived failed replacement: %v", err)
	}
}

func TestPublishRejectsUnsyncedStagedFilesBeforeRemovingCommitMarker(t *testing.T) {
	for _, failedFile := range []string{"tar", "json"} {
		t.Run(failedFile, func(t *testing.T) {
			outputDir, stagedTarPath, metadata := publicationFixture(t)
			jsonPath := filepath.Join(outputDir, "rootfs.json")
			err := publishWithOps(outputDir, stagedTarPath, metadata, publishOps{
				rename: os.Rename,
				syncFile: func(file *os.File) error {
					if strings.Contains(filepath.Base(file.Name()), "."+failedFile+".tmp") {
						return fmt.Errorf("injected %s sync failure", failedFile)
					}
					return file.Sync()
				},
				syncDir: syncDirectory,
			})
			if err == nil || !strings.Contains(err.Error(), "sync staged rootfs "+failedFile) {
				t.Fatalf("error = %v", err)
			}
			if got, readErr := os.ReadFile(jsonPath); readErr != nil || !bytes.Contains(got, []byte("old valid digest")) {
				t.Fatalf("commit marker changed before staged files were durable: %q, %v", got, readErr)
			}
		})
	}
}

func TestPublishSyncsDirectoryAfterEachCommitStep(t *testing.T) {
	for _, tc := range []struct {
		name          string
		failSync      int
		wantTar       string
		wantJSON      bool
		wantErrorStep string
	}{
		{
			name: "marker removal", failSync: 1, wantTar: "old tar",
			wantErrorStep: "removing rootfs metadata",
		},
		{
			name: "tar rename", failSync: 2, wantTar: "new tar",
			wantErrorStep: "publishing rootfs tar",
		},
		{
			name: "marker rename", failSync: 3, wantTar: "new tar", wantJSON: true,
			wantErrorStep: "publishing rootfs metadata",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputDir, stagedTarPath, metadata := publicationFixture(t)
			syncCalls := 0
			err := publishWithOps(outputDir, stagedTarPath, metadata, publishOps{
				rename:   os.Rename,
				syncFile: (*os.File).Sync,
				syncDir: func(path string) error {
					syncCalls++
					if syncCalls == tc.failSync {
						return fmt.Errorf("injected directory sync failure")
					}
					return syncDirectory(path)
				},
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErrorStep) {
				t.Fatalf("error = %v", err)
			}
			tarData, readErr := os.ReadFile(filepath.Join(outputDir, "rootfs.tar"))
			if readErr != nil || string(tarData) != tc.wantTar {
				t.Fatalf("rootfs.tar = %q, %v", tarData, readErr)
			}
			_, statErr := os.Stat(filepath.Join(outputDir, "rootfs.json"))
			if tc.wantJSON && statErr != nil {
				t.Fatalf("rootfs.json missing: %v", statErr)
			}
			if !tc.wantJSON && !os.IsNotExist(statErr) {
				t.Fatalf("rootfs.json exists before its durable commit: %v", statErr)
			}
		})
	}
}

func publicationFixture(t *testing.T) (string, string, fwextract.Metadata) {
	t.Helper()
	outputDir := filepath.Join(t.TempDir(), "output")
	if err := os.Mkdir(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "rootfs.tar"), []byte("old tar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(outputDir, "rootfs.json"),
		[]byte(`{"tar_digest":"old valid digest"}`+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	stagedTar, err := createOutputTemp(outputDir, ".rootfs-*.tar.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stagedTar.Write([]byte("new tar")); err != nil {
		t.Fatal(err)
	}
	if err := stagedTar.Close(); err != nil {
		t.Fatal(err)
	}
	return outputDir, stagedTar.Name(), fwextract.Metadata{
		SourceFirmwareDigest: strings.Repeat("a", 64),
		Platform:             "TEST",
		Version:              "1",
		NestedArtifactPath:   "firmware.bin/rootfs",
		TarDigest:            strings.Repeat("b", 64),
		EntryCount:           1,
	}
}

func commandRootFS(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := cpio.Newc.Writer(&archive)
	if err := writer.WriteRecord(cpio.StaticFile("usr/bin/mcad", "vendor", 0o755)); err != nil {
		t.Fatal(err)
	}
	if err := cpio.WriteTrailer(writer); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func commandUBNTImage(data []byte) []byte {
	header := make([]byte, 268)
	copy(header, "UBNTtest")
	binary.BigEndian.PutUint32(header[260:264], crc32.ChecksumIEEE(header[:260]))
	part := make([]byte, 56)
	copy(part, "PART")
	copy(part[4:20], "agent")
	binary.BigEndian.PutUint32(part[48:52], uint32(len(data)))
	binary.BigEndian.PutUint32(part[52:56], uint32(len(data)))
	record := append(part, data...)
	crc := make([]byte, 8)
	binary.BigEndian.PutUint32(crc[:4], crc32.ChecksumIEEE(record))
	image := append(append(header, record...), crc...)
	image = append(image, []byte("ENDS")...)
	return append(image, make([]byte, 260)...)
}
