package firmware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	if calls != 1 {
		t.Fatalf("download calls = %d, want one", calls)
	}
	if !bytes.Equal(first, body) || !bytes.Equal(second, body) {
		t.Fatal("cached bytes changed")
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
				if filepath.Ext(entry.Name()) == ".bin" {
					t.Fatalf("invalid image became a cache entry: %s", entry.Name())
				}
			}
		})
	}
}
