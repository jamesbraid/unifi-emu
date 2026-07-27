// Command fwextract derives auditable behaviour and capability evidence from
// official UniFi firmware without retaining vendor binaries in the repository.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jamesbraid/unifi-emu/internal/firmware"
)

const catalogueLimit = 16 << 20

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			*s = append(*s, trimmed)
		}
	}
	return nil
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "fwextract:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, client *http.Client) error {
	flags := flag.NewFlagSet("fwextract", flag.ContinueOnError)
	flags.SetOutput(stderr)
	defaults := firmware.DefaultLimits()
	var platforms stringList
	var (
		imagePath = flags.String("image", "", "analyze one local firmware image")
		indexPath = flags.String("index", "", "read a firmware-latest JSON file")
		indexURL  = flags.String("index-url", "", "fetch a firmware-latest JSON URL")
		cacheDir  = flags.String("cache", "", "verified firmware download cache")
		output    = flags.String("out", "-", "manifest output path, or - for stdout")
		sha       = flags.String("sha256", "", "select or verify a SHA-256 checksum")
		version   = flags.String("version", "", "firmware version in local-image mode")
		livePath  = flags.String("live", "", "optional sanitized mca-dump JSON or text")
		capsPath  = flags.String("capability-bits", "", "controller capability_bits.json for decoding live bitmap values")
		rootTar   = flags.String("rootfs-tar", "", "write deterministic extracted rootfs.tar")
		rootJSON  = flags.String("rootfs-json", "", "write rootfs provenance metadata")
		all       = flags.Bool("all", false, "select every unique catalogue image")
		maxImage  = flags.Int64("max-image-bytes", defaults.MaxImageBytes, "maximum input image bytes")
		maxExpand = flags.Int64("max-expanded-bytes", defaults.MaxExpandedBytes, "maximum total decompressed bytes")
		maxFiles  = flags.Int("max-artifacts", defaults.MaxArtifacts, "maximum decoded artifacts")
		maxDepth  = flags.Int("max-depth", defaults.MaxDepth, "maximum nested decoder depth")
		minString = flags.Int("min-string-length", defaults.MinStringLength, "minimum printable string length")
	)
	flags.Var(&platforms, "platform", "platform to select; repeat or comma-separate")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	inputModes := countNonEmpty(*imagePath, *indexPath, *indexURL)
	if inputModes != 1 {
		return errors.New("choose exactly one of -image, -index, or -index-url")
	}
	limits := firmware.Limits{
		MaxImageBytes: *maxImage, MaxExpandedBytes: *maxExpand,
		MaxArtifacts: *maxFiles, MaxDepth: *maxDepth, MinStringLength: *minString,
	}
	if *maxImage <= 0 || *maxExpand <= 0 || *maxFiles <= 0 || *maxDepth <= 0 || *minString <= 0 {
		return errors.New("all extraction limits must be positive")
	}
	if *all && (len(platforms) > 0 || *sha != "") {
		return errors.New("-all cannot be combined with -platform or -sha256")
	}
	if (*rootTar == "") != (*rootJSON == "") {
		return errors.New("-rootfs-tar and -rootfs-json must be supplied together")
	}
	if *rootTar != "" && filepath.Clean(*rootTar) == filepath.Clean(*rootJSON) {
		return errors.New("-rootfs-tar and -rootfs-json must use distinct output paths")
	}
	manifest := firmware.Manifest{
		SchemaVersion: 2, Generator: "fwextract", GeneratorVersion: "2",
		Limits: firmware.ManifestLimits{
			MaxImageBytes: *maxImage, MaxExpandedBytes: *maxExpand,
			MaxArtifacts: *maxFiles, MaxDepth: *maxDepth, MinStringLength: *minString,
		},
	}
	var runErr error
	var rootfs *firmware.RootFSBundle
	if *imagePath != "" {
		if *sha == "" || *version == "" || len(platforms) == 0 {
			return errors.New("local image mode requires -sha256, -version, and at least one -platform")
		}
		data, sum, err := firmware.LoadLocal(*imagePath, strings.ToLower(*sha), *maxImage)
		if err != nil {
			return err
		}
		evidence, bundle, rootErr := firmware.AnalyzeImageWithRootFS(firmware.SelectedImage{
			SHA256: sum, Version: *version, Platforms: platforms, Size: int64(len(data)),
		}, data, limits)
		manifest.Images = append(manifest.Images, evidence)
		rootfs = bundle
		if *rootTar != "" && rootErr != nil {
			return fmt.Errorf("build rootfs bundle: %w", rootErr)
		}
	} else {
		if *cacheDir == "" {
			return errors.New("catalogue mode requires -cache")
		}
		catalogue, err := readCatalogue(ctx, *indexPath, *indexURL, client)
		if err != nil {
			return err
		}
		records, err := firmware.ParseCatalogue(bytes.NewReader(catalogue))
		if err != nil {
			return err
		}
		selected, err := firmware.SelectRecords(records, firmware.Selection{
			Platforms: platforms, SHA256: strings.ToLower(*sha), All: *all,
		})
		if err != nil {
			return err
		}
		if *rootTar != "" && len(selected) != 1 {
			return fmt.Errorf("rootfs output requires exactly one selected firmware image, got %d", len(selected))
		}
		cache := firmware.Cache{Dir: *cacheDir, Client: client, MaxImageBytes: *maxImage}
		successes := 0
		for _, image := range selected {
			data, err := cache.Fetch(ctx, image)
			if err != nil {
				manifest.Images = append(manifest.Images, firmware.ImageEvidence{
					SHA256: image.SHA256, Version: image.Version, Platforms: image.Platforms,
					SourceURL: image.URL, SourceURLs: image.URLs, Size: image.Size,
					Failures: []firmware.Failure{{Stage: "source", Artifact: "firmware.bin", Error: err.Error()}},
				})
				continue
			}
			successes++
			evidence, bundle, rootErr := firmware.AnalyzeImageWithRootFS(image, data, limits)
			manifest.Images = append(manifest.Images, evidence)
			rootfs = bundle
			if *rootTar != "" && rootErr != nil {
				return fmt.Errorf("build rootfs bundle: %w", rootErr)
			}
		}
		if successes == 0 {
			runErr = errors.New("all selected firmware images failed")
		}
	}
	if *livePath != "" {
		file, err := os.Open(*livePath)
		if err != nil {
			return fmt.Errorf("open live evidence: %w", err)
		}
		err = firmware.AttachLiveEvidence(&manifest, file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return fmt.Errorf("close live evidence: %w", closeErr)
		}
	}
	if *capsPath != "" {
		if *livePath == "" {
			return errors.New("-capability-bits requires -live JSON evidence")
		}
		file, err := os.Open(*capsPath)
		if err != nil {
			return fmt.Errorf("open capability definitions: %w", err)
		}
		err = firmware.AttachCapabilityDefinitions(&manifest, file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return fmt.Errorf("close capability definitions: %w", closeErr)
		}
	}
	if err := writeManifest(*output, stdout, manifest); err != nil {
		return err
	}
	if *rootTar != "" {
		if rootfs == nil {
			return errors.New("selected firmware contains no decoded root filesystem")
		}
		if err := writeRootFSBundle(*rootTar, *rootJSON, rootfs); err != nil {
			return err
		}
	}
	return runErr
}

func readCatalogue(ctx context.Context, path, url string, client *http.Client) ([]byte, error) {
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open firmware catalogue: %w", err)
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, catalogueLimit+1))
		if err != nil {
			return nil, fmt.Errorf("read firmware catalogue: %w", err)
		}
		if len(data) > catalogueLimit {
			return nil, errors.New("firmware catalogue exceeds 16 MiB limit")
		}
		return data, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create catalogue request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch firmware catalogue: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch firmware catalogue: HTTP %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, catalogueLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read firmware catalogue: %w", err)
	}
	if len(data) > catalogueLimit {
		return nil, errors.New("firmware catalogue exceeds 16 MiB limit")
	}
	return data, nil
}

func writeManifest(path string, stdout io.Writer, manifest firmware.Manifest) error {
	if path == "-" {
		return firmware.WriteManifest(stdout, manifest)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".fwextract-*.tmp")
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := firmware.WriteManifest(temp, manifest); err != nil {
		temp.Close()
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return fmt.Errorf("chmod manifest: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close manifest: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish manifest: %w", err)
	}
	return nil
}

func writeRootFSBundle(tarPath, jsonPath string, bundle *firmware.RootFSBundle) error {
	tarTemp, err := createOutputTemp(tarPath, ".rootfs-*.tar.tmp")
	if err != nil {
		return err
	}
	tarTempPath := tarTemp.Name()
	defer os.Remove(tarTempPath)
	if _, err := tarTemp.Write(bundle.Tar); err != nil {
		tarTemp.Close()
		return fmt.Errorf("write rootfs tar: %w", err)
	}
	if err := tarTemp.Chmod(0o644); err != nil {
		tarTemp.Close()
		return fmt.Errorf("chmod rootfs tar: %w", err)
	}
	if err := tarTemp.Close(); err != nil {
		return fmt.Errorf("close rootfs tar: %w", err)
	}

	jsonTemp, err := createOutputTemp(jsonPath, ".rootfs-*.json.tmp")
	if err != nil {
		return err
	}
	jsonTempPath := jsonTemp.Name()
	defer os.Remove(jsonTempPath)
	if err := firmware.WriteRootFSMetadata(jsonTemp, bundle.Metadata); err != nil {
		jsonTemp.Close()
		return fmt.Errorf("write rootfs metadata: %w", err)
	}
	if err := jsonTemp.Chmod(0o644); err != nil {
		jsonTemp.Close()
		return fmt.Errorf("chmod rootfs metadata: %w", err)
	}
	if err := jsonTemp.Close(); err != nil {
		return fmt.Errorf("close rootfs metadata: %w", err)
	}
	if err := os.Rename(tarTempPath, tarPath); err != nil {
		return fmt.Errorf("publish rootfs tar: %w", err)
	}
	// Metadata is the commit marker: consumers ignore a tar without rootfs.json.
	if err := os.Rename(jsonTempPath, jsonPath); err != nil {
		return fmt.Errorf("publish rootfs metadata: %w", err)
	}
	return nil
}

func createOutputTemp(path, pattern string) (*os.File, error) {
	if path == "-" {
		return nil, errors.New("rootfs outputs must be files")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return nil, fmt.Errorf("create output: %w", err)
	}
	return file, nil
}

func countNonEmpty(values ...string) int {
	count := 0
	for _, value := range values {
		if value != "" {
			count++
		}
	}
	return count
}
