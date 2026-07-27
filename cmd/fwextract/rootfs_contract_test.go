package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/u-root/u-root/pkg/cpio"
)

func TestRunRejectsSameRootFSOutputPath(t *testing.T) {
	var archive bytes.Buffer
	writer := cpio.Newc.Writer(&archive)
	if err := writer.WriteRecord(cpio.StaticFile("usr/bin/mcad", "vendor", 0o755)); err != nil {
		t.Fatal(err)
	}
	if err := cpio.WriteTrailer(writer); err != nil {
		t.Fatal(err)
	}
	image := commandUBNTImage(archive.Bytes())
	sum := fmt.Sprintf("%x", sha256.Sum256(image))
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "firmware.bin")
	outputPath := filepath.Join(dir, "rootfs.bundle")
	if err := os.WriteFile(imagePath, image, 0o600); err != nil {
		t.Fatal(err)
	}

	err := run(context.Background(), []string{
		"-image", imagePath, "-sha256", sum, "-platform", "TEST", "-version", "1",
		"-rootfs-tar", outputPath, "-rootfs-json", outputPath,
	}, &bytes.Buffer{}, &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("same rootfs output path error = %v", err)
	}
}
