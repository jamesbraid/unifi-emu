package firmware

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"
)

type RootFSMetadata struct {
	SourceFirmwareDigest string `json:"source_firmware_digest"`
	Platform             string `json:"platform"`
	Version              string `json:"version"`
	NestedArtifactPath   string `json:"nested_artifact_path"`
	TarDigest            string `json:"tar_digest"`
	EntryCount           int    `json:"entry_count"`
}

type RootFSBundle struct {
	Tar      []byte
	Metadata RootFSMetadata
}

func buildRootFSBundle(result DecodeResult, image SelectedImage) (*RootFSBundle, error) {
	root, ok := selectRootFS(result.Roots)
	if !ok {
		return nil, fmt.Errorf("firmware contains no decoded root filesystem")
	}
	entries, err := normalizeRootEntries(root.Entries)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	epoch := time.Unix(0, 0).UTC()
	for _, entry := range entries {
		name := entry.Path
		header := &tar.Header{
			Name: name, Mode: entry.Mode & 0o7777, ModTime: epoch,
			AccessTime: epoch, ChangeTime: epoch, Format: tar.FormatPAX,
		}
		switch entry.Kind {
		case "regular":
			header.Typeflag, header.Size = tar.TypeReg, int64(len(entry.Data))
		case "directory":
			header.Typeflag = tar.TypeDir
			header.Name = strings.TrimSuffix(name, "/") + "/"
		case "symlink":
			header.Typeflag, header.Linkname = tar.TypeSymlink, entry.Linkname
		case "hardlink":
			header.Typeflag, header.Linkname = tar.TypeLink, entry.Linkname
		case "char-device":
			header.Typeflag = tar.TypeChar
			header.Devmajor, header.Devminor = entry.DeviceMajor, entry.DeviceMinor
		case "block-device":
			header.Typeflag = tar.TypeBlock
			header.Devmajor, header.Devminor = entry.DeviceMajor, entry.DeviceMinor
		case "fifo":
			header.Typeflag = tar.TypeFifo
		default:
			return nil, fmt.Errorf("unsupported rootfs entry kind %q for %q", entry.Kind, entry.Path)
		}
		if err := writer.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write rootfs tar header %q: %w", name, err)
		}
		if entry.Kind == "regular" {
			if _, err := writer.Write(entry.Data); err != nil {
				return nil, fmt.Errorf("write rootfs tar file %q: %w", name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close rootfs tar: %w", err)
	}
	tarBytes := output.Bytes()
	platforms := uniqueStrings(append([]string(nil), image.Platforms...))
	platform := ""
	if len(platforms) > 0 {
		platform = platforms[0]
	}
	return &RootFSBundle{
		Tar: tarBytes,
		Metadata: RootFSMetadata{
			SourceFirmwareDigest: image.SHA256,
			Platform:             platform,
			Version:              image.Version,
			NestedArtifactPath:   root.Artifact,
			TarDigest:            fmt.Sprintf("%x", sha256.Sum256(tarBytes)),
			EntryCount:           len(entries),
		},
	}, nil
}

func selectRootFS(roots []decodedRoot) (decodedRoot, bool) {
	bestScore, best := -1, decodedRoot{}
	for _, root := range roots {
		score := len(root.Entries)
		if strings.Contains(strings.ToLower(root.Artifact), "rootfs") {
			score += 100_000
		}
		for _, entry := range root.Entries {
			if entry.Kind == "regular" && (entry.Path == "usr/bin/mcad" ||
				strings.HasSuffix(entry.Path, "/usr/bin/mcad")) {
				score += 1_000_000
				break
			}
		}
		if score > bestScore || score == bestScore && root.Artifact < best.Artifact {
			bestScore, best = score, root
		}
	}
	return best, bestScore >= 0
}

func normalizeRootEntries(input []decodedFile) ([]decodedFile, error) {
	// Archive extraction semantics are last-entry-wins. Real initramfs archives
	// repeat directory records, so collapse duplicates to the final filesystem
	// state before sorting the deterministic tar.
	byPath := make(map[string]decodedFile, len(input))
	for _, entry := range input {
		byPath[entry.Path] = entry
	}
	entries := make([]decodedFile, 0, len(byPath))
	for _, entry := range byPath {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b decodedFile) int { return cmpStrings(a.Path, b.Path) })
	byKind := make(map[string]string, len(entries))
	linkKeyGroups := make(map[string][]int)
	for index := range entries {
		entry := &entries[index]
		if !safeArchivePath(entry.Path) {
			return nil, fmt.Errorf("unsafe rootfs path %q", entry.Path)
		}
		byKind[entry.Path] = entry.Kind
		if entry.Kind == "hardlink" && !safeArchivePath(entry.Linkname) {
			return nil, fmt.Errorf("unsafe rootfs hardlink target %q", entry.Linkname)
		}
		if entry.LinkKey != "" && entry.Kind == "regular" {
			linkKeyGroups[entry.LinkKey] = append(linkKeyGroups[entry.LinkKey], index)
		}
	}
	for _, entry := range entries {
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			if kind, exists := byKind[parent]; exists && kind != "directory" {
				return nil, fmt.Errorf("rootfs entry %q is below non-directory %q", entry.Path, parent)
			}
		}
	}

	// A tar hardlink must refer to a preceding member, while the bundle contract
	// also requires canonical path ordering. Model both explicit archive links
	// and CPIO inode groups as equivalence classes, then make the
	// lexicographically first path the payload-bearing member.
	parent := make([]int, len(entries))
	for index := range parent {
		parent[index] = index
	}
	var find func(int) int
	find = func(index int) int {
		if parent[index] != index {
			parent[index] = find(parent[index])
		}
		return parent[index]
	}
	union := func(a, b int) {
		a, b = find(a), find(b)
		if a != b {
			parent[b] = a
		}
	}
	indexByPath := make(map[string]int, len(entries))
	for index, entry := range entries {
		indexByPath[entry.Path] = index
	}
	for index, entry := range entries {
		if entry.Kind != "hardlink" {
			continue
		}
		target, ok := indexByPath[entry.Linkname]
		if !ok {
			return nil, fmt.Errorf("rootfs hardlink %q has missing target %q", entry.Path, entry.Linkname)
		}
		union(index, target)
	}
	for _, indexes := range linkKeyGroups {
		for _, index := range indexes[1:] {
			union(indexes[0], index)
		}
	}
	groups := make(map[int][]int)
	for index := range entries {
		groups[find(index)] = append(groups[find(index)], index)
	}
	for _, indexes := range groups {
		hasLink := len(indexes) > 1
		for _, index := range indexes {
			hasLink = hasLink || entries[index].Kind == "hardlink"
		}
		if !hasLink {
			continue
		}
		var content []byte
		var payloadMode int64
		foundPayload := false
		for _, index := range indexes {
			if entries[index].Kind == "regular" {
				content = entries[index].Data
				payloadMode = entries[index].Mode
				foundPayload = true
				break
			}
		}
		if !foundPayload {
			return nil, fmt.Errorf("rootfs hardlink group at %q has no regular payload", entries[indexes[0]].Path)
		}
		target := entries[indexes[0]].Path
		entries[indexes[0]].Kind = "regular"
		entries[indexes[0]].Linkname = ""
		entries[indexes[0]].Data = content
		entries[indexes[0]].Mode = payloadMode
		for _, index := range indexes[1:] {
			entries[index].Kind = "hardlink"
			entries[index].Linkname = target
			entries[index].Data = nil
		}
	}
	return entries, nil
}

func WriteRootFSMetadata(w io.Writer, metadata RootFSMetadata) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(metadata)
}
