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
	"time"
)

const defaultMaxImageBytes int64 = 512 << 20

type Cache struct {
	Dir           string
	Client        *http.Client
	MaxImageBytes int64
}

func (c Cache) Fetch(ctx context.Context, image SelectedImage) ([]byte, error) {
	if !validSHA256(image.SHA256) {
		return nil, fmt.Errorf("invalid expected SHA-256 %q", image.SHA256)
	}
	if c.Dir == "" {
		return nil, fmt.Errorf("cache directory is required")
	}
	limit := c.MaxImageBytes
	if limit <= 0 {
		limit = defaultMaxImageBytes
	}
	if image.Size > limit {
		return nil, fmt.Errorf("image size %d exceeds limit %d", image.Size, limit)
	}
	path := filepath.Join(c.Dir, image.SHA256+".bin")
	if data, err := readLimitedFile(path, limit); err == nil {
		if err := verifyImage(data, image, limit); err == nil {
			return data, nil
		}
	}
	if image.URL == "" {
		return nil, fmt.Errorf("image %s is not cached and has no download URL", image.SHA256)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, image.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("create firmware request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download firmware: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download firmware: HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read firmware response: %w", err)
	}
	if err := verifyImage(data, image, limit); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create firmware cache: %w", err)
	}
	tmp, err := os.CreateTemp(c.Dir, ".firmware-*.partial")
	if err != nil {
		return nil, fmt.Errorf("create firmware cache entry: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write firmware cache entry: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("chmod firmware cache entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close firmware cache entry: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, fmt.Errorf("publish firmware cache entry: %w", err)
	}
	return data, nil
}

func readLimitedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file size %d exceeds limit %d", info.Size(), limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds limit %d", limit)
	}
	return data, nil
}

func LoadLocal(path, expectedSHA string, maxBytes int64) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxImageBytes
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open firmware image: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read firmware image: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("image size %d exceeds limit %d", len(data), maxBytes)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	if expectedSHA != "" && sum != expectedSHA {
		return nil, "", fmt.Errorf("firmware SHA-256 mismatch: got %s, want %s", sum, expectedSHA)
	}
	return data, sum, nil
}

func verifyImage(data []byte, image SelectedImage, limit int64) error {
	if int64(len(data)) > limit {
		return fmt.Errorf("image size %d exceeds limit %d", len(data), limit)
	}
	if image.Size > 0 && int64(len(data)) != image.Size {
		return fmt.Errorf("firmware size mismatch: got %d, want %d", len(data), image.Size)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(data))
	if got != image.SHA256 {
		return fmt.Errorf("firmware SHA-256 mismatch: got %s, want %s", got, image.SHA256)
	}
	return nil
}
