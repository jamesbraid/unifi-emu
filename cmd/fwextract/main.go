// Command fwextract publishes a deterministic root filesystem from one
// verified firmware image.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jamesbraid/unifi-emu/fwextract"
)

// Keep this aligned with fwextract's input limit.
const maxFirmwareBytes int64 = 1 << 30

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fwextract:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("fwextract", flag.ContinueOnError)
	flags.SetOutput(stderr)
	imagePath := flags.String("image", "", "local firmware image")
	digest := flags.String("sha256", "", "expected firmware SHA-256")
	platform := flags.String("platform", "", "firmware platform")
	version := flags.String("version", "", "firmware version")
	outputDir := flags.String("out", "", "output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	for name, value := range map[string]string{
		"image": *imagePath, "sha256": *digest, "platform": *platform,
		"version": *version, "output": *outputDir,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	image, err := readFirmware(*imagePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	tarTemp, err := createOutputTemp(*outputDir, ".rootfs-*.tar.tmp")
	if err != nil {
		return err
	}
	tarTempPath := tarTemp.Name()
	defer func() {
		// The staged file has either been renamed or is abandoned cleanup.
		_ = os.Remove(tarTempPath)
	}()
	metadata, err := fwextract.Extract(image, fwextract.Source{
		Digest: strings.ToLower(*digest), Platform: *platform, Version: *version,
	}, tarTemp)
	if err != nil {
		return closeFile(tarTemp, "rootfs tar", err)
	}
	if err := tarTemp.Chmod(0o644); err != nil {
		return closeFile(tarTemp, "rootfs tar", fmt.Errorf("chmod rootfs tar: %w", err))
	}
	if err := tarTemp.Close(); err != nil {
		return fmt.Errorf("close rootfs tar: %w", err)
	}
	return publish(*outputDir, tarTempPath, metadata)
}

func readFirmware(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat firmware image: %w", err)
	}
	if info.Size() > maxFirmwareBytes {
		return nil, fmt.Errorf(
			"firmware image size %d exceeds limit %d",
			info.Size(), maxFirmwareBytes,
		)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read firmware image: %w", err)
	}
	return image, nil
}

func publish(outputDir, tarTempPath string, metadata fwextract.Metadata) error {
	return publishWithRename(outputDir, tarTempPath, metadata, os.Rename)
}

func publishWithRename(
	outputDir string,
	tarTempPath string,
	metadata fwextract.Metadata,
	rename func(string, string) error,
) error {
	return publishWithOps(outputDir, tarTempPath, metadata, publishOps{
		rename: rename, syncFile: (*os.File).Sync, syncDir: syncDirectory,
	})
}

type publishOps struct {
	rename   func(string, string) error
	syncFile func(*os.File) error
	syncDir  func(string) error
}

func publishWithOps(
	outputDir string,
	tarTempPath string,
	metadata fwextract.Metadata,
	ops publishOps,
) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	tarPath := filepath.Join(outputDir, "rootfs.tar")
	jsonPath := filepath.Join(outputDir, "rootfs.json")

	tarTemp, err := os.OpenFile(tarTempPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open staged rootfs tar: %w", err)
	}
	if err := ops.syncFile(tarTemp); err != nil {
		return closeFile(tarTemp, "staged rootfs tar",
			fmt.Errorf("sync staged rootfs tar: %w", err))
	}
	if err := tarTemp.Close(); err != nil {
		return fmt.Errorf("close staged rootfs tar: %w", err)
	}

	jsonTemp, err := createOutputTemp(outputDir, ".rootfs-*.json.tmp")
	if err != nil {
		return err
	}
	jsonTempPath := jsonTemp.Name()
	defer func() {
		// The staged file has either been renamed or is abandoned cleanup.
		_ = os.Remove(jsonTempPath)
	}()
	if err := fwextract.WriteMetadata(jsonTemp, metadata); err != nil {
		return closeFile(jsonTemp, "rootfs metadata",
			fmt.Errorf("write rootfs metadata: %w", err))
	}
	if err := jsonTemp.Chmod(0o644); err != nil {
		return closeFile(jsonTemp, "rootfs metadata",
			fmt.Errorf("chmod rootfs metadata: %w", err))
	}
	if err := ops.syncFile(jsonTemp); err != nil {
		return closeFile(jsonTemp, "rootfs metadata",
			fmt.Errorf("sync staged rootfs json: %w", err))
	}
	if err := jsonTemp.Close(); err != nil {
		return fmt.Errorf("close rootfs metadata: %w", err)
	}
	markerRemoved := false
	if err := os.Remove(jsonPath); err == nil {
		markerRemoved = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("remove old rootfs metadata: %w", err)
	}
	if markerRemoved {
		if err := ops.syncDir(outputDir); err != nil {
			return fmt.Errorf("sync output directory after removing rootfs metadata: %w", err)
		}
	}
	if err := ops.rename(tarTempPath, tarPath); err != nil {
		return fmt.Errorf("publish rootfs tar: %w", err)
	}
	if err := ops.syncDir(outputDir); err != nil {
		return fmt.Errorf("sync output directory after publishing rootfs tar: %w", err)
	}
	// rootfs.json is the commit marker for consumers.
	if err := ops.rename(jsonTempPath, jsonPath); err != nil {
		return fmt.Errorf("publish rootfs metadata: %w", err)
	}
	if err := ops.syncDir(outputDir); err != nil {
		return fmt.Errorf("sync output directory after publishing rootfs metadata: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		return closeFile(dir, "directory", err)
	}
	return dir.Close()
}

func closeFile(file *os.File, description string, primary error) error {
	closeErr := file.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close %s: %w", description, closeErr)
	}
	return errors.Join(primary, closeErr)
}

func createOutputTemp(dir, pattern string) (*os.File, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, fmt.Errorf("create output: %w", err)
	}
	return file, nil
}
