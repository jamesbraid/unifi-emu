package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/u-root/u-root/pkg/cpio"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRunLocalImageWritesManifest(t *testing.T) {
	image := commandUBNTImage([]byte("fw_caps\x00setparam\x00"))
	sum := fmt.Sprintf("%x", sha256.Sum256(image))
	path := filepath.Join(t.TempDir(), "firmware.bin")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{
		"-image", path,
		"-sha256", sum,
		"-platform", "TEST",
		"-version", "1.2.3",
	}, &stdout, &stderr, nil)
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	for _, want := range []string{
		`"schema_version": 2`, `"TEST"`, `"format": "ubnt"`,
		`"value": "fw_caps"`, `"value": "setparam"`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("manifest missing %s:\n%s", want, stdout.String())
		}
	}
}

func TestRunRequiresOneInputModeAndLocalMetadata(t *testing.T) {
	for name, args := range map[string][]string{
		"no input":        nil,
		"two inputs":      {"-image", "one", "-index", "two"},
		"local metadata":  {"-image", "one"},
		"zero limit":      {"-image", "one", "-sha256", strings.Repeat("a", 64), "-version", "1", "-platform", "TEST", "-max-depth", "0"},
		"mixed selection": {"-index", "index", "-cache", "cache", "-all", "-platform", "TEST"},
		"rootfs pair":     {"-image", "one", "-rootfs-tar", "rootfs.tar"},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, nil)
			if err == nil {
				t.Fatal("accepted invalid command arguments")
			}
			if name == "zero limit" && !strings.Contains(err.Error(), "must be positive") {
				t.Fatalf("limit error = %v", err)
			}
		})
	}
}

func TestRunWritesRootFSConsumerBundle(t *testing.T) {
	var archive bytes.Buffer
	writer := cpio.Newc.Writer(&archive)
	if err := writer.WriteRecord(cpio.Directory("usr", 0o755)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRecord(cpio.StaticFile("usr/bin/mcad", "vendor", 0o755)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteRecord(cpio.Symlink("bin", "usr/bin")); err != nil {
		t.Fatal(err)
	}
	if err := cpio.WriteTrailer(writer); err != nil {
		t.Fatal(err)
	}
	image := commandUBNTImage(archive.Bytes())
	sum := fmt.Sprintf("%x", sha256.Sum256(image))
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "firmware.bin")
	tarPath := filepath.Join(dir, "rootfs.tar")
	jsonPath := filepath.Join(dir, "rootfs.json")
	if err := os.WriteFile(imagePath, image, 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{
		"-image", imagePath, "-sha256", sum, "-platform", "TEST", "-version", "1.2.3",
		"-rootfs-tar", tarPath, "-rootfs-json", jsonPath,
	}, &bytes.Buffer{}, &bytes.Buffer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tarData, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), `"source_firmware_digest": "`+sum+`"`) ||
		!strings.Contains(string(meta), `"nested_artifact_path": "firmware.bin/agent"`) ||
		!strings.Contains(string(meta), fmt.Sprintf(`"tar_digest": "%x"`, sha256.Sum256(tarData))) ||
		bytes.Contains(tarData, []byte("fwextract")) {
		t.Fatalf("rootfs bundle metadata=%s", meta)
	}
}

func TestRunCatalogueDownloadsSelectedImage(t *testing.T) {
	image := commandUBNTImage([]byte("fw_caps\x00"))
	sum := fmt.Sprintf("%x", sha256.Sum256(image))
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "firmware-latest.json")
	index := fmt.Sprintf(`{"_embedded":{"firmware":[{
		"product":"unifi-firmware","platform":"TEST","version":"v1.2.3+4",
		"file_size":%d,"sha256_checksum":%q,
		"_links":{"data":{"href":"https://example/firmware.bin"}}
	}]}}`, len(image), sum)
	if err := os.WriteFile(indexPath, []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK",
			Body: io.NopCloser(bytes.NewReader(image)), Header: make(http.Header),
			Request: request,
		}, nil
	})}
	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"-index", indexPath, "-cache", filepath.Join(dir, "cache"), "-platform", "TEST",
	}, &output, &bytes.Buffer{}, client); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(output.String(), `"version": "1.2.3.4"`) {
		t.Fatalf("calls=%d manifest=%s", calls, output.String())
	}
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
