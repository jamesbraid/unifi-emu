package fwextract

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

var xzMagic = []byte{0xfd, '7', 'z', 'X', 'Z', 0}

const defaultMaxImageBytes = 1 << 30

type limits struct {
	MaxImageBytes    int64
	MaxExpandedBytes int64
	MaxArtifacts     int
	MaxDepth         int
}

func defaultLimits() limits {
	return limits{
		MaxImageBytes:    defaultMaxImageBytes,
		MaxExpandedBytes: 4 << 30,
		MaxArtifacts:     100000,
		MaxDepth:         12,
	}
}

func (l limits) withDefaults() limits {
	defaults := defaultLimits()
	if l.MaxImageBytes <= 0 {
		l.MaxImageBytes = defaults.MaxImageBytes
	}
	if l.MaxExpandedBytes <= 0 {
		l.MaxExpandedBytes = defaults.MaxExpandedBytes
	}
	if l.MaxArtifacts <= 0 {
		l.MaxArtifacts = defaults.MaxArtifacts
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = defaults.MaxDepth
	}
	return l
}

type decodedFile struct {
	Path              string
	Data              []byte
	Mode              int64
	Kind              string
	Linkname          string
	LinkKey           string
	DeviceMajor       int64
	DeviceMinor       int64
	MaterializedBytes int64
}

type decodeResult struct {
	Roots    []decodedRoot
	Failures []decodeFailure
}

type decodeFailure struct {
	Stage    string
	Artifact string
	Error    string
}

type decodedRoot struct {
	Artifact string
	Entries  []decodedFile
}

type decoderState struct {
	limits    limits
	result    decodeResult
	expanded  int64
	artifacts int
}

func decode(data []byte, name string, limits limits) decodeResult {
	state := decoderState{limits: limits.withDefaults()}
	if name == "" {
		name = "firmware.bin"
	}
	if int64(len(data)) > state.limits.MaxImageBytes {
		state.fail(name, "source", fmt.Errorf("image size %d exceeds limit %d", len(data), state.limits.MaxImageBytes))
		return state.result
	}
	state.walk(name, data, "", 0)
	return state.result
}

func (s *decoderState) walk(name string, data []byte, compression string, depth int) {
	if depth > s.limits.MaxDepth {
		s.fail(name, "decode", fmt.Errorf("nesting exceeds limit %d", s.limits.MaxDepth))
		return
	}
	if !s.reserveArtifact(name, "decode") {
		return
	}

	if compression != "" && compression != "none" {
		decoded, err := s.decompress(data, compression)
		if err != nil {
			s.fail(name, "decompress", err)
			return
		}
		s.walk(name, decoded, "", depth+1)
		return
	}

	format := detectFormat(data)
	switch format {
	case "ubnt":
		children, err := parseUBNT(data, s.limits)
		if err != nil {
			s.fail(name, "ubnt", err)
			return
		}
		for _, child := range children {
			s.walk(joinArtifact(name, child.Name), child.Data, child.Compression, depth+1)
		}
	case "uimage":
		child, err := parseUImage(data)
		if err != nil {
			s.fail(name, "uimage", err)
			return
		}
		s.walk(joinArtifact(name, child.Name), child.Data, child.Compression, depth+1)
	case "fit":
		children, err := parseFIT(data, s.limits)
		if err != nil {
			s.fail(name, "fit", err)
			return
		}
		if len(children) == 0 {
			return
		}
		for _, child := range children {
			if s.artifacts >= s.limits.MaxArtifacts {
				s.fail(name, "fit", fmt.Errorf("artifact count exceeds limit %d", s.limits.MaxArtifacts))
				break
			}
			s.walk(joinArtifact(name, child.Name), child.Data, child.Compression, depth+1)
		}
	case "gzip", "bzip2", "lz4", "zstd", "xz", "lzop":
		decoded, err := s.decompress(data, format)
		if err != nil {
			s.fail(name, "decompress", err)
			return
		}
		s.walk(name, decoded, "", depth+1)
	case "cpio":
		archiveLimits, err := s.archiveLimits()
		if err != nil {
			s.fail(name, "cpio", err)
			return
		}
		files, err := parseCPIO(data, archiveLimits)
		if err != nil {
			s.fail(name, "cpio", err)
			return
		}
		if err := s.chargeEntries(files); err != nil {
			s.fail(name, "cpio", err)
			return
		}
		s.processArchive(name, files, depth)
	case "squashfs":
		archiveLimits, err := s.archiveLimits()
		if err != nil {
			s.fail(name, "squashfs", err)
			return
		}
		files, err := parseSquashFS(data, archiveLimits)
		if err != nil {
			s.fail(name, "squashfs", err)
			return
		}
		if err := s.chargeEntries(files); err != nil {
			s.fail(name, "squashfs", err)
			return
		}
		s.processArchive(name, files, depth)
	case "tar":
		archiveLimits, err := s.archiveLimits()
		if err != nil {
			s.fail(name, "tar", err)
			return
		}
		files, err := parseTar(data, archiveLimits)
		if err != nil {
			s.fail(name, "tar", err)
			return
		}
		if err := s.chargeEntries(files); err != nil {
			s.fail(name, "tar", err)
			return
		}
		s.processArchive(name, files, depth)
	default:
		archiveLimits, hasArchiveBudget := s.archiveLimitsIfAvailable()
		if hasArchiveBudget {
			if _, files, ok := findEmbeddedSquashFS(data, archiveLimits); ok {
				if err := s.chargeEntries(files); err != nil {
					s.fail(name, "squashfs", err)
					return
				}
				squashPath := joinArtifact(name, "rootfs.squashfs")
				if !s.reserveArtifact(squashPath, "squashfs") {
					return
				}
				s.processArchive(squashPath, files, depth)
				return
			}
		}
		if hasArchiveBudget {
			if _, files, ok := findEmbeddedCPIO(data, archiveLimits); ok {
				if err := s.chargeEntries(files); err != nil {
					s.fail(name, "cpio", err)
					return
				}
				cpioPath := joinArtifact(name, "initramfs")
				if !s.reserveArtifact(cpioPath, "cpio") {
					return
				}
				s.processArchive(cpioPath, files, depth)
				return
			}
		}
		remaining := s.limits.MaxExpandedBytes - s.expanded
		if _, decoded, ok := findEmbeddedLZMA(data, remaining); ok {
			embeddedName := joinArtifact(name, "embedded.lzma")
			s.expanded += int64(len(decoded))
			if !s.reserveArtifact(embeddedName, "lzma") {
				return
			}
			s.walk(embeddedName, decoded, "", depth+1)
			return
		}
		if embeddedOffset := findEmbeddedXZ(data); embeddedOffset >= 0 {
			s.walk(joinArtifact(name, "embedded.xz"), data[embeddedOffset:], "", depth+1)
			return
		}
	}
}

func (s *decoderState) addFile(name string, data []byte, depth int) {
	format := detectFormat(data)
	if format == "ubnt" && !validUBNTHeader(data) {
		format = "opaque"
	}
	if format != "opaque" && format != "esp32" {
		s.walk(name, data, "", depth)
	}
}

func (s *decoderState) processArchive(name string, files []decodedFile, depth int) {
	if hasRootFSMarker(files) {
		s.result.Roots = append(s.result.Roots, decodedRoot{Artifact: name, Entries: files})
		return
	}
	rootsBefore := len(s.result.Roots)
	for _, file := range files {
		if file.Kind == "regular" {
			s.addFile(joinArtifact(name, file.Path), file.Data, depth+1)
		}
	}
	if len(s.result.Roots) == rootsBefore && rootFSNameHint(name) {
		s.result.Roots = append(s.result.Roots, decodedRoot{Artifact: name, Entries: files})
	}
}

func hasRootFSMarker(entries []decodedFile) bool {
	for _, entry := range entries {
		switch entry.Path {
		case "usr/bin/mcad", "bin/busybox", "init", "sbin/init":
			return true
		}
	}
	return false
}

func rootFSNameHint(artifact string) bool {
	return strings.Contains(strings.ToLower(artifact), "rootfs")
}

func (s *decoderState) reserveArtifact(name, stage string) bool {
	if s.artifacts >= s.limits.MaxArtifacts {
		s.fail(name, stage, fmt.Errorf("artifact count exceeds limit %d", s.limits.MaxArtifacts))
		return false
	}
	s.artifacts++
	return true
}

func (s *decoderState) archiveLimits() (limits, error) {
	budget, ok := s.archiveLimitsIfAvailable()
	if !ok {
		return limits{}, fmt.Errorf("expanded bytes exceed limit %d", s.limits.MaxExpandedBytes)
	}
	return budget, nil
}

func (s *decoderState) archiveLimitsIfAvailable() (limits, bool) {
	remaining := s.limits.MaxExpandedBytes - s.expanded
	if remaining <= 0 {
		return limits{}, false
	}
	budget := s.limits
	budget.MaxExpandedBytes = remaining
	return budget, true
}

func (s *decoderState) chargeEntries(entries []decodedFile) error {
	for _, entry := range entries {
		if entry.MaterializedBytes < 0 ||
			entry.MaterializedBytes > s.limits.MaxExpandedBytes-s.expanded {
			return fmt.Errorf("expanded bytes exceed limit %d", s.limits.MaxExpandedBytes)
		}
		s.expanded += entry.MaterializedBytes
	}
	return nil
}

func (s *decoderState) decompress(data []byte, kind string) ([]byte, error) {
	var reader io.Reader
	var closeReader func() error
	switch kind {
	case "gzip":
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open gzip stream: %w", err)
		}
		reader = gz
		closeReader = gz.Close
	case "bzip2":
		reader = bzip2.NewReader(bytes.NewReader(data))
	case "lz4":
		reader = lz4.NewReader(bytes.NewReader(data))
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(data), zstd.WithDecoderMaxMemory(uint64(s.limits.MaxExpandedBytes)))
		if err != nil {
			return nil, fmt.Errorf("open Zstandard stream: %w", err)
		}
		defer zr.Close()
		reader = zr
	case "lzma":
		const maxLZMADictionary = 64 << 20
		lr, err := (lzma.ReaderConfig{DictCap: maxLZMADictionary}).NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open LZMA stream: %w", err)
		}
		reader = lr
	case "xz":
		xr, err := (xz.ReaderConfig{DictCap: 64 << 20}).NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open XZ stream: %w", err)
		}
		reader = xr
	case "lzo", "lzop":
		remaining := s.limits.MaxExpandedBytes - s.expanded
		if remaining < 0 {
			return nil, fmt.Errorf("expanded bytes exceed limit %d", s.limits.MaxExpandedBytes)
		}
		decoded, err := decompressLZOP(data, remaining)
		if err != nil {
			return nil, err
		}
		s.expanded += int64(len(decoded))
		if s.expanded > s.limits.MaxExpandedBytes {
			return nil, fmt.Errorf("expanded bytes exceed limit %d", s.limits.MaxExpandedBytes)
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("unsupported compression %q", kind)
	}
	remaining := s.limits.MaxExpandedBytes - s.expanded
	if remaining < 0 {
		return nil, closeDecompressor(closeReader,
			fmt.Errorf("expanded bytes exceed limit %d", s.limits.MaxExpandedBytes))
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, remaining+1))
	if err != nil {
		return nil, closeDecompressor(closeReader, fmt.Errorf("decompress %s: %w", kind, err))
	}
	if err := closeDecompressor(closeReader, nil); err != nil {
		return nil, fmt.Errorf("decompress %s: %w", kind, err)
	}
	s.expanded += int64(len(decoded))
	if s.expanded > s.limits.MaxExpandedBytes {
		return nil, fmt.Errorf("expanded bytes exceed limit %d", s.limits.MaxExpandedBytes)
	}
	return decoded, nil
}

func closeDecompressor(closeReader func() error, primary error) error {
	if closeReader == nil {
		return primary
	}
	closeErr := closeReader()
	if closeErr != nil {
		closeErr = fmt.Errorf("close compressed stream: %w", closeErr)
	}
	return errors.Join(primary, closeErr)
}

func (s *decoderState) fail(artifact, stage string, err error) {
	s.result.Failures = append(s.result.Failures, decodeFailure{
		Stage: stage, Artifact: artifact, Error: err.Error(),
	})
}

func detectFormat(data []byte) string {
	switch {
	case len(data) >= 4 && (string(data[:4]) == "UBNT" || string(data[:4]) == "OPEN"):
		return "ubnt"
	case len(data) >= 4 && binary.BigEndian.Uint32(data[:4]) == uImageMagic:
		return "uimage"
	case len(data) >= 4 && binary.BigEndian.Uint32(data[:4]) == fdtMagic:
		return "fit"
	case len(data) >= 6 && (string(data[:6]) == "070701" || string(data[:6]) == "070702"):
		return "cpio"
	case len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b:
		return "gzip"
	case len(data) >= 3 && string(data[:3]) == "BZh":
		return "bzip2"
	case len(data) >= 4 && binary.LittleEndian.Uint32(data[:4]) == 0x184d2204:
		return "lz4"
	case len(data) >= 4 && binary.LittleEndian.Uint32(data[:4]) == 0xfd2fb528:
		return "zstd"
	case len(data) >= len(xzMagic) && bytes.Equal(data[:len(xzMagic)], xzMagic):
		return "xz"
	case len(data) >= len(lzopMagic) && bytes.Equal(data[:len(lzopMagic)], lzopMagic):
		return "lzop"
	case len(data) >= 4 && string(data[:4]) == "hsqs":
		return "squashfs"
	case len(data) >= 262 && string(data[257:262]) == "ustar":
		return "tar"
	case len(data) >= 4 && data[0] == 0xe9:
		return "esp32"
	default:
		return "opaque"
	}
}

func parseTar(data []byte, limits limits) ([]decodedFile, error) {
	limits = limits.withDefaults()
	reader := tar.NewReader(bytes.NewReader(data))
	var files []decodedFile
	var expanded int64
	entries := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		entries++
		if entries > limits.MaxArtifacts {
			return nil, fmt.Errorf("tar entry count exceeds limit %d", limits.MaxArtifacts)
		}
		entryName := header.Name
		for strings.HasPrefix(entryName, "./") {
			entryName = strings.TrimPrefix(entryName, "./")
		}
		if header.Typeflag == tar.TypeDir {
			entryName = strings.TrimRight(entryName, "/")
			if entryName == "" {
				continue
			}
		}
		if !safeArchivePath(entryName) {
			return nil, fmt.Errorf("unsafe tar path %q", header.Name)
		}
		entry := decodedFile{Path: entryName, Mode: header.Mode & 0o7777}
		switch header.Typeflag {
		case tar.TypeReg:
			entry.Kind = "regular"
		case tar.TypeDir:
			entry.Kind = "directory"
		case tar.TypeSymlink:
			entry.Kind, entry.Linkname = "symlink", header.Linkname
		case tar.TypeLink:
			linkname := header.Linkname
			for strings.HasPrefix(linkname, "./") {
				linkname = strings.TrimPrefix(linkname, "./")
			}
			if !safeArchivePath(linkname) {
				return nil, fmt.Errorf("unsafe tar hardlink target %q", header.Linkname)
			}
			entry.Kind, entry.Linkname = "hardlink", linkname
		case tar.TypeChar:
			entry.Kind = "char-device"
			entry.DeviceMajor, entry.DeviceMinor = header.Devmajor, header.Devminor
		case tar.TypeBlock:
			entry.Kind = "block-device"
			entry.DeviceMajor, entry.DeviceMinor = header.Devmajor, header.Devminor
		case tar.TypeFifo:
			entry.Kind = "fifo"
		default:
			return nil, fmt.Errorf("unsupported tar entry type %q for %q", header.Typeflag, header.Name)
		}
		if entry.Kind == "regular" {
			if header.Size < 0 || header.Size > limits.MaxExpandedBytes-expanded {
				return nil, fmt.Errorf("tar expanded bytes exceed limit %d", limits.MaxExpandedBytes)
			}
			content, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil {
				return nil, fmt.Errorf("read tar entry %q: %w", header.Name, err)
			}
			if int64(len(content)) != header.Size {
				return nil, fmt.Errorf("short tar entry %q: got %d bytes, want %d", header.Name, len(content), header.Size)
			}
			expanded += header.Size
			entry.Data = content
			entry.MaterializedBytes = header.Size
		}
		files = append(files, entry)
	}
}

func joinArtifact(parent, child string) string {
	return strings.TrimSuffix(parent, "/") + "/" + strings.TrimPrefix(child, "/")
}

func findEmbeddedSquashFS(data []byte, limits limits) (int, []decodedFile, bool) {
	const maxCandidates = 32
	candidates := 0
	for searchFrom := 1; searchFrom < len(data); {
		relative := bytes.Index(data[searchFrom:], []byte("hsqs"))
		if relative < 0 {
			return 0, nil, false
		}
		candidates++
		if candidates > maxCandidates {
			return 0, nil, false
		}
		offset := searchFrom + relative
		if files, err := parseSquashFS(data[offset:], limits); err == nil {
			return offset, files, true
		}
		searchFrom = offset + 1
	}
	return 0, nil, false
}

func findEmbeddedCPIO(data []byte, limits limits) (int, []decodedFile, bool) {
	const maxCandidates = 64
	candidates := 0
	for searchFrom := 1; searchFrom < len(data); {
		relative := bytes.Index(data[searchFrom:], []byte("07070"))
		if relative < 0 {
			return 0, nil, false
		}
		candidates++
		if candidates > maxCandidates {
			return 0, nil, false
		}
		offset := searchFrom + relative
		if files, err := parseEmbeddedCPIO(data[offset:], limits); err == nil && len(files) > 0 {
			return offset, files, true
		}
		searchFrom = offset + 1
	}
	return 0, nil, false
}

func findEmbeddedLZMA(data []byte, maxExpanded int64) (int, []byte, bool) {
	if maxExpanded <= 0 {
		return 0, nil, false
	}
	for offset := len(data) - 13; offset >= 1; offset-- {
		properties := data[offset]
		if properties >= 9*5*5 {
			continue
		}
		dictionary := binary.LittleEndian.Uint32(data[offset+1 : offset+5])
		if dictionary < 4096 || dictionary > 64<<20 || dictionary&(dictionary-1) != 0 {
			continue
		}
		unknownSize := true
		for _, value := range data[offset+5 : offset+13] {
			unknownSize = unknownSize && value == 0xff
		}
		if unknownSize {
			reader, err := (lzma.ReaderConfig{DictCap: 64 << 20}).NewReader(
				bytes.NewReader(data[offset:]))
			if err != nil {
				continue
			}
			decoded, err := io.ReadAll(io.LimitReader(reader, maxExpanded+1))
			if err == nil && int64(len(decoded)) <= maxExpanded {
				return offset, decoded, true
			}
		}
	}
	return 0, nil, false
}

func findEmbeddedXZ(data []byte) int {
	end := len(data)
	for end >= 4 && bytes.Equal(data[end-4:end], []byte{0, 0, 0, 0}) {
		end -= 4
	}
	if end < 2 || !bytes.Equal(data[end-2:end], []byte{'Y', 'Z'}) {
		return -1
	}
	for searchEnd := end; searchEnd > 1; {
		relative := bytes.LastIndex(data[1:searchEnd], xzMagic)
		if relative < 0 {
			return -1
		}
		offset := 1 + relative
		if _, err := (xz.ReaderConfig{DictCap: 64 << 20}).NewReader(bytes.NewReader(data[offset:end])); err == nil {
			return offset
		}
		searchEnd = offset
	}
	return -1
}
