package fwgrab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCacheFetchVerifiesAndReusesImage(t *testing.T) {
	body := []byte("firmware")
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}
	cache := Cache{Dir: t.TempDir(), Client: client, MaxImageBytes: 100}
	rec := SelectedImage{SHA256: sum, Size: int64(len(body)), URL: "https://example/firmware.bin"}

	first, err := cache.Fetch(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Fetch(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(cache.Dir, sum+".bin")
	if calls != 1 {
		t.Fatalf("download calls = %d, want one", calls)
	}
	if first != wantPath || second != wantPath {
		t.Fatalf("cache paths = %q, %q; want %q", first, second, wantPath)
	}
	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("cached bytes = %q", got)
	}
}

func TestCacheFetchStreamsResponseIntoPartialFile(t *testing.T) {
	body := bytes.Repeat([]byte("firmware"), 1024)
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	dir := t.TempDir()
	stream := &partialAwareReader{data: bytes.NewReader(body), dir: dir}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(stream),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}
	cache := Cache{Dir: dir, Client: client, MaxImageBytes: int64(len(body))}

	path, err := cache.Fetch(context.Background(), SelectedImage{
		SHA256: sum, Size: int64(len(body)), URL: "https://example/firmware.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, sum+".bin") {
		t.Fatalf("cache path = %q", path)
	}
}

func TestCacheFetchReportsDirectorySyncFailureAfterPublishing(t *testing.T) {
	body := []byte("firmware")
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	dir := t.TempDir()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}
	cache := Cache{Dir: dir, Client: client, MaxImageBytes: int64(len(body))}

	_, err := cache.fetch(context.Background(), SelectedImage{
		SHA256: sum, Size: int64(len(body)), URL: "https://example/firmware.bin",
	}, func(path string) error {
		if path != dir {
			t.Fatalf("synced directory = %q, want %q", path, dir)
		}
		return fmt.Errorf("injected sync failure")
	})
	if err == nil || !strings.Contains(err.Error(), "sync firmware cache directory") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, sum+".bin")); statErr != nil {
		t.Fatalf("published cache entry missing after directory sync failure: %v", statErr)
	}
}

func TestCacheFetchRejectsCorruptAndOversizedData(t *testing.T) {
	goodSum := fmt.Sprintf("%x", sha256.Sum256([]byte("good")))
	for name, tc := range map[string]struct {
		body []byte
		max  int64
	}{
		"checksum": {body: []byte("bad"), max: 100},
		"limit":    {body: []byte("too large"), max: 2},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(tc.body)),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			})}
			cache := Cache{Dir: dir, Client: client, MaxImageBytes: tc.max}
			_, err := cache.Fetch(context.Background(), SelectedImage{
				SHA256: goodSum, Size: int64(len(tc.body)), URL: "https://example/firmware.bin",
			})
			if err == nil {
				t.Fatal("accepted invalid download")
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if filepath.Ext(entry.Name()) == ".bin" || filepath.Ext(entry.Name()) == ".partial" {
					t.Fatalf("invalid image left a cache entry: %s", entry.Name())
				}
			}
		})
	}
}

type partialAwareReader struct {
	data    *bytes.Reader
	dir     string
	checked bool
}

func (r *partialAwareReader) Read(p []byte) (int, error) {
	if !r.checked {
		r.checked = true
		entries, err := os.ReadDir(r.dir)
		if err != nil {
			return 0, err
		}
		found := false
		for _, entry := range entries {
			found = found || filepath.Ext(entry.Name()) == ".partial"
		}
		if !found {
			return 0, fmt.Errorf("response read before partial cache file existed")
		}
	}
	return r.data.Read(p)
}
