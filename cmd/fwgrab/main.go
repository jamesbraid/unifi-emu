// Command fwgrab downloads SHA-256-verified firmware into a local cache.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jamesbraid/unifi-emu/internal/fwgrab"
)

const (
	catalogueLimit       = 16 << 20
	defaultMaxImageBytes = 1 << 30
)

type stringList []string

type provenanceRecord struct {
	Path      string   `json:"path"`
	Digest    string   `json:"digest"`
	Version   string   `json:"version"`
	Platforms []string `json:"platforms"`
}

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
		fmt.Fprintln(os.Stderr, "fwgrab:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, client *http.Client) error {
	flags := flag.NewFlagSet("fwgrab", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var platforms stringList
	indexPath := flags.String("index", "", "firmware-latest JSON file")
	indexURL := flags.String("index-url", "", "firmware-latest JSON URL")
	cacheDir := flags.String("cache", "", "verified firmware cache")
	digest := flags.String("sha256", "", "firmware SHA-256 to select")
	all := flags.Bool("all", false, "download every unique catalogue image")
	jsonOutput := flags.Bool("json", false, "write one JSON provenance record per image")
	maxImage := flags.Int64("max-image-bytes", defaultMaxImageBytes, "maximum firmware image bytes")
	flags.Var(&platforms, "platform", "platform to select; repeat or comma-separate")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if countNonEmpty(*indexPath, *indexURL) != 1 {
		return errors.New("choose exactly one of -index or -index-url")
	}
	if *cacheDir == "" {
		return errors.New("cache directory is required")
	}
	if *maxImage <= 0 {
		return errors.New("-max-image-bytes must be positive")
	}
	if *all && (len(platforms) > 0 || *digest != "") {
		return errors.New("-all cannot be combined with -platform or -sha256")
	}

	catalogue, err := readCatalogue(ctx, *indexPath, *indexURL, client)
	if err != nil {
		return err
	}
	records, err := fwgrab.ParseCatalogue(bytes.NewReader(catalogue))
	if err != nil {
		return err
	}
	images, err := fwgrab.SelectRecords(records, fwgrab.Selection{
		Platforms: platforms, SHA256: strings.ToLower(*digest), All: *all,
	})
	if err != nil {
		return err
	}
	cache := fwgrab.Cache{Dir: *cacheDir, Client: client, MaxImageBytes: *maxImage}
	var downloadErrors []error
	for _, image := range images {
		path, err := cache.Fetch(ctx, image)
		if err != nil {
			downloadErrors = append(downloadErrors, fmt.Errorf("%s: %w", image.SHA256, err))
			continue
		}
		if *jsonOutput {
			record := provenanceRecord{
				Path: path, Digest: image.SHA256,
				Version: image.Version, Platforms: image.Platforms,
			}
			if err := json.NewEncoder(stdout).Encode(record); err != nil {
				return fmt.Errorf("write provenance record: %w", err)
			}
			continue
		}
		if _, err := fmt.Fprintln(stdout, path); err != nil {
			return fmt.Errorf("write cache path: %w", err)
		}
	}
	return errors.Join(downloadErrors...)
}

func readCatalogue(ctx context.Context, path, url string, client *http.Client) ([]byte, error) {
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open firmware catalogue: %w", err)
		}
		data, readErr := readLimitedCatalogue(file)
		closeErr := file.Close()
		if closeErr != nil {
			closeErr = fmt.Errorf("close firmware catalogue: %w", closeErr)
		}
		return data, errors.Join(readErr, closeErr)
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
	return readLimitedCatalogue(response.Body)
}

func readLimitedCatalogue(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, catalogueLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read firmware catalogue: %w", err)
	}
	if len(data) > catalogueLimit {
		return nil, errors.New("firmware catalogue exceeds 16 MiB limit")
	}
	return data, nil
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
