package firmware

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

type FirmwareRecord struct {
	Product  string
	Platform string
	Version  string
	SHA256   string
	Size     int64
	URL      string
}

type SelectedImage struct {
	SHA256    string
	Version   string
	Platforms []string
	Size      int64
	URL       string
	URLs      []string
}

type Selection struct {
	Platforms []string
	SHA256    string
	All       bool
}

func ParseCatalogue(r io.Reader) ([]FirmwareRecord, error) {
	var envelope struct {
		Embedded struct {
			Firmware []struct {
				Product  string `json:"product"`
				Platform string `json:"platform"`
				Version  string `json:"version"`
				SHA256   string `json:"sha256_checksum"`
				Size     int64  `json:"file_size"`
				Links    struct {
					Data struct {
						Href string `json:"href"`
					} `json:"data"`
				} `json:"_links"`
			} `json:"firmware"`
		} `json:"_embedded"`
	}
	if err := json.NewDecoder(r).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode firmware catalogue: %w", err)
	}
	var records []FirmwareRecord
	for _, raw := range envelope.Embedded.Firmware {
		if raw.Product != "unifi-firmware" {
			continue
		}
		record := FirmwareRecord{
			Product: raw.Product, Platform: raw.Platform,
			Version: normalizeVersion(raw.Version), SHA256: strings.ToLower(raw.SHA256),
			Size: raw.Size, URL: raw.Links.Data.Href,
		}
		if record.Platform == "" || record.Version == "" || !validSHA256(record.SHA256) ||
			record.Size <= 0 || record.URL == "" {
			continue
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		return nil, errors.New("firmware catalogue contains no usable unifi-firmware records")
	}
	return records, nil
}

func SelectRecords(records []FirmwareRecord, selection Selection) ([]SelectedImage, error) {
	if selection.SHA256 != "" && !validSHA256(selection.SHA256) {
		return nil, fmt.Errorf("invalid SHA-256 %q", selection.SHA256)
	}
	wanted := make(map[string]bool, len(selection.Platforms))
	for _, platform := range selection.Platforms {
		wanted[strings.ToUpper(strings.TrimSpace(platform))] = true
	}
	if !selection.All && selection.SHA256 == "" && len(wanted) == 0 {
		return nil, errors.New("select at least one platform or SHA-256, or use all")
	}

	bySHA := make(map[string]*SelectedImage)
	for _, record := range records {
		if selection.SHA256 != "" && !strings.EqualFold(record.SHA256, selection.SHA256) {
			continue
		}
		if len(wanted) > 0 && !wanted[strings.ToUpper(record.Platform)] {
			continue
		}
		image := bySHA[record.SHA256]
		if image == nil {
			image = &SelectedImage{
				SHA256: record.SHA256, Version: record.Version,
				Size: record.Size,
			}
			bySHA[record.SHA256] = image
		} else if image.Version != record.Version || image.Size != record.Size {
			return nil, fmt.Errorf("conflicting catalogue metadata for SHA-256 %s", record.SHA256)
		}
		image.Platforms = append(image.Platforms, record.Platform)
		image.URLs = append(image.URLs, record.URL)
	}
	if len(bySHA) == 0 {
		return nil, errors.New("firmware selection matched no images")
	}
	out := make([]SelectedImage, 0, len(bySHA))
	for _, image := range bySHA {
		image.Platforms = uniqueStrings(image.Platforms)
		image.URLs = uniqueStrings(image.URLs)
		image.URL = image.URLs[0]
		out = append(out, *image)
	}
	slices.SortFunc(out, func(a, b SelectedImage) int { return cmpStrings(a.SHA256, b.SHA256) })
	return out, nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func normalizeVersion(version string) string {
	return strings.Replace(strings.TrimPrefix(version, "v"), "+", ".", 1)
}
