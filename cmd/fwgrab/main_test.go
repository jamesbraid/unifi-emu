package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRunDownloadsSelectedFirmwareIntoVerifiedCache(t *testing.T) {
	body := []byte("firmware")
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "firmware-latest.json")
	if err := os.WriteFile(indexPath, catalogue(sum, len(body), "TEST", "https://example/firmware.bin"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(http.StatusOK, body, request), nil
	})}

	var stdout, stderr bytes.Buffer
	cacheDir := filepath.Join(dir, "cache")
	err := run(context.Background(), []string{
		"-index", indexPath, "-cache", cacheDir, "-platform", "TEST",
	}, &stdout, &stderr, client)
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	wantPath := filepath.Join(cacheDir, sum+".bin")
	if stdout.String() != wantPath+"\n" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantPath+"\n")
	}
	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("cache contents = %q", got)
	}
}

func TestRunJSONWritesOneProvenanceRecordPerImage(t *testing.T) {
	body := []byte("firmware")
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "firmware-latest.json")
	index := fmt.Sprintf(`{"_embedded":{"firmware":[
		{"product":"unifi-firmware","platform":"TEST","version":"v1.2.3+4","file_size":%d,"sha256_checksum":%q,"_links":{"data":{"href":"https://example/firmware.bin"}}},
		{"product":"unifi-firmware","platform":"TEST-ALIAS","version":"v1.2.3+4","file_size":%d,"sha256_checksum":%q,"_links":{"data":{"href":"https://example/firmware.bin"}}}
	]}}`, len(body), sum, len(body), sum)
	if err := os.WriteFile(indexPath, []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(http.StatusOK, body, request), nil
	})}

	var stdout bytes.Buffer
	cacheDir := filepath.Join(dir, "cache")
	if err := run(context.Background(), []string{
		"-index", indexPath, "-cache", cacheDir, "-all", "-json",
	}, &stdout, &bytes.Buffer{}, client); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Path      string   `json:"path"`
		Digest    string   `json:"digest"`
		Version   string   `json:"version"`
		Platforms []string `json:"platforms"`
	}
	decoder := json.NewDecoder(&stdout)
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	if got.Path != filepath.Join(cacheDir, sum+".bin") ||
		got.Digest != sum ||
		got.Version != "1.2.3.4" ||
		strings.Join(got.Platforms, ",") != "TEST,TEST-ALIAS" {
		t.Fatalf("record = %#v", got)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("trailing output error = %v, value = %#v\n%s", err, extra, stdout.String())
	}
}

func TestRunAllDownloadsEveryUniqueImage(t *testing.T) {
	one, two := []byte("one"), []byte("two")
	sumOne := fmt.Sprintf("%x", sha256.Sum256(one))
	sumTwo := fmt.Sprintf("%x", sha256.Sum256(two))
	index := fmt.Sprintf(`{"_embedded":{"firmware":[
		{"product":"unifi-firmware","platform":"ONE","version":"v1","file_size":%d,"sha256_checksum":%q,"_links":{"data":{"href":"https://example/one"}}},
		{"product":"unifi-firmware","platform":"TWO","version":"v2","file_size":%d,"sha256_checksum":%q,"_links":{"data":{"href":"https://example/two"}}}
	]}}`, len(one), sumOne, len(two), sumTwo)
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.json")
	if err := os.WriteFile(indexPath, []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/one":
			return response(http.StatusOK, one, request), nil
		case "/two":
			return response(http.StatusOK, two, request), nil
		default:
			t.Fatalf("unexpected URL %s", request.URL)
			return nil, nil
		}
	})}

	cacheDir := filepath.Join(dir, "cache")
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{
		"-index", indexPath, "-cache", cacheDir, "-all",
	}, &stdout, &bytes.Buffer{}, client); err != nil {
		t.Fatal(err)
	}
	for _, sum := range []string{sumOne, sumTwo} {
		path := filepath.Join(cacheDir, sum+".bin")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
		if !strings.Contains(stdout.String(), path+"\n") {
			t.Fatalf("stdout missing %q:\n%s", path, stdout.String())
		}
	}
}

func TestRunAllAttemptsEveryDownloadAfterFailure(t *testing.T) {
	successBody := []byte("success")
	successSum := fmt.Sprintf("%x", sha256.Sum256(successBody))
	failureSum := strings.Repeat("0", 64)
	index := fmt.Sprintf(`{"_embedded":{"firmware":[
		{"product":"unifi-firmware","platform":"FAIL","version":"v1","file_size":1,"sha256_checksum":%q,"_links":{"data":{"href":"https://example/fail"}}},
		{"product":"unifi-firmware","platform":"OK","version":"v1","file_size":%d,"sha256_checksum":%q,"_links":{"data":{"href":"https://example/ok"}}}
	]}}`, failureSum, len(successBody), successSum)
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.json")
	if err := os.WriteFile(indexPath, []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	var paths []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		if request.URL.Path == "/fail" {
			return response(http.StatusBadGateway, nil, request), nil
		}
		return response(http.StatusOK, successBody, request), nil
	})}

	cacheDir := filepath.Join(dir, "cache")
	err := run(context.Background(), []string{
		"-index", indexPath, "-cache", cacheDir, "-all",
	}, &bytes.Buffer{}, &bytes.Buffer{}, client)
	if err == nil {
		t.Fatal("reported success after a failed download")
	}
	if got := strings.Join(paths, ","); got != "/fail,/ok" {
		t.Fatalf("download paths = %q, want both images attempted", got)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, successSum+".bin")); err != nil {
		t.Fatalf("successful image was not cached: %v", err)
	}
}

func TestRunFetchesRemoteCatalogue(t *testing.T) {
	body := []byte("firmware")
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	index := catalogue(sum, len(body), "TEST", "https://example/firmware.bin")
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/index":
			return response(http.StatusOK, index, request), nil
		case "/firmware.bin":
			return response(http.StatusOK, body, request), nil
		default:
			t.Fatalf("unexpected URL %s", request.URL)
			return nil, nil
		}
	})}

	err := run(context.Background(), []string{
		"-index-url", "https://example/index", "-cache", t.TempDir(), "-platform", "TEST",
	}, &bytes.Buffer{}, &bytes.Buffer{}, client)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunRequiresOneCatalogueAndCache(t *testing.T) {
	for name, args := range map[string][]string{
		"catalogue":       nil,
		"two catalogues":  {"-index", "one", "-index-url", "two", "-cache", "cache", "-all"},
		"cache":           {"-index", "index", "-all"},
		"selection":       {"-index", "index", "-cache", "cache"},
		"mixed selection": {"-index", "index", "-cache", "cache", "-all", "-platform", "TEST"},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}, nil)
			if err == nil {
				t.Fatal("accepted invalid arguments")
			}
		})
	}
}

func catalogue(sum string, size int, platform, url string) []byte {
	return []byte(fmt.Sprintf(`{"_embedded":{"firmware":[{
		"product":"unifi-firmware","platform":%q,"version":"v1.2.3+4",
		"file_size":%d,"sha256_checksum":%q,
		"_links":{"data":{"href":%q}}
	}]}}`, platform, size, sum, url))
}

func response(status int, body []byte, request *http.Request) *http.Response {
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header), Request: request,
	}
}
