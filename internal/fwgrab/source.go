package fwgrab

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const defaultMaxImageBytes int64 = 1 << 30

type Cache struct {
	Dir           string
	Client        *http.Client
	MaxImageBytes int64
}

func (c Cache) Fetch(ctx context.Context, image SelectedImage) (string, error) {
	return c.fetch(ctx, image, syncDirectory)
}

func (c Cache) fetch(
	ctx context.Context,
	image SelectedImage,
	syncDir func(string) error,
) (string, error) {
	if !validSHA256(image.SHA256) {
		return "", fmt.Errorf("invalid expected SHA-256 %q", image.SHA256)
	}
	if c.Dir == "" {
		return "", fmt.Errorf("cache directory is required")
	}
	limit := c.MaxImageBytes
	if limit <= 0 {
		limit = defaultMaxImageBytes
	}
	if image.Size > limit {
		return "", fmt.Errorf("image size %d exceeds limit %d", image.Size, limit)
	}
	path := filepath.Join(c.Dir, image.SHA256+".bin")
	if err := verifyCachedFile(path, image, limit); err == nil {
		return path, nil
	}
	if image.URL == "" {
		return "", fmt.Errorf("image %s is not cached and has no download URL", image.SHA256)
	}

	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, image.URL, nil)
	if err != nil {
		return "", fmt.Errorf("create firmware request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download firmware: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download firmware: HTTP %s", resp.Status)
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return "", fmt.Errorf("create firmware cache: %w", err)
	}
	tmp, err := os.CreateTemp(c.Dir, ".firmware-*.partial")
	if err != nil {
		return "", fmt.Errorf("create firmware cache entry: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// The partial file has either been renamed or is abandoned cleanup.
		_ = os.Remove(tmpName)
	}()

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", closeFile(tmp, "firmware cache entry",
			fmt.Errorf("download firmware body: %w", err))
	}
	if err := verifyImage(size, fmt.Sprintf("%x", hasher.Sum(nil)), image, limit); err != nil {
		return "", closeFile(tmp, "firmware cache entry", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return "", closeFile(tmp, "firmware cache entry",
			fmt.Errorf("chmod firmware cache entry: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return "", closeFile(tmp, "firmware cache entry",
			fmt.Errorf("sync firmware cache entry: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close firmware cache entry: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("publish firmware cache entry: %w", err)
	}
	if err := syncDir(c.Dir); err != nil {
		return "", fmt.Errorf("sync firmware cache directory: %w", err)
	}
	return path, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		return closeFile(dir, "firmware cache directory", err)
	}
	return dir.Close()
}

func verifyCachedFile(path string, image SelectedImage, limit int64) (err error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		err = closeFile(file, "cached firmware", err)
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > limit {
		return fmt.Errorf("file size %d exceeds limit %d", info.Size(), limit)
	}
	if image.Size > 0 && info.Size() != image.Size {
		return fmt.Errorf("firmware size mismatch: got %d, want %d", info.Size(), image.Size)
	}
	hasher := sha256.New()
	size, err := io.Copy(hasher, io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	return verifyImage(size, fmt.Sprintf("%x", hasher.Sum(nil)), image, limit)
}

func closeFile(file *os.File, description string, primary error) error {
	closeErr := file.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close %s: %w", description, closeErr)
	}
	return errors.Join(primary, closeErr)
}

func verifyImage(size int64, digest string, image SelectedImage, limit int64) error {
	if size > limit {
		return fmt.Errorf("image size %d exceeds limit %d", size, limit)
	}
	if image.Size > 0 && size != image.Size {
		return fmt.Errorf("firmware size mismatch: got %d, want %d", size, image.Size)
	}
	if digest != image.SHA256 {
		return fmt.Errorf("firmware SHA-256 mismatch: got %s, want %s", digest, image.SHA256)
	}
	return nil
}
