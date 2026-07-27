package firmware

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/u-root/u-root/pkg/cpio"
)

const (
	newcHeaderSize     = 110
	maxCPIONameBytes   = 1 << 20
	newcNumericFields  = 13
	newcNumericHexSize = 8
)

// parseCPIO delegates newc record parsing to u-root. The checks here are
// extraction policy: bounded reads, preserving the raw archive path, and
// limiting both entry count and expanded bytes.
func parseCPIO(data []byte, limits Limits) ([]decodedFile, error) {
	limits = limits.withDefaults()
	if err := preflightCPIO(data, limits); err != nil {
		return nil, err
	}
	reader := cpio.Newc.Reader(bytes.NewReader(data))
	var files []decodedFile
	var expanded int64
	entries := 0

	for {
		record, err := reader.ReadRecord()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read CPIO record: %w", err)
		}
		entries++
		if entries > limits.MaxArtifacts {
			return nil, fmt.Errorf("CPIO entry count exceeds limit %d", limits.MaxArtifacts)
		}
		rawName, err := rawCPIOName(data, record.RecPos)
		if err != nil {
			return nil, err
		}
		if rawName != record.Name || !safeArchivePath(rawName) {
			return nil, fmt.Errorf("unsafe CPIO path %q", rawName)
		}
		entry := decodedFile{
			Path: record.Name, Offset: record.FilePos,
			Mode: int64(record.Mode & 0o7777),
		}
		switch record.Mode & cpio.S_IFMT {
		case cpio.S_IFDIR:
			entry.Kind = "directory"
		case cpio.S_IFREG:
			entry.Kind = "regular"
			if record.NLink > 1 {
				entry.LinkKey = fmt.Sprintf("cpio:%d:%d:%d", record.Major, record.Minor, record.Ino)
			}
		case cpio.S_IFLNK:
			entry.Kind = "symlink"
		default:
			continue
		}
		if entry.Kind == "regular" || entry.Kind == "symlink" {
			if record.FileSize > uint64(limits.MaxExpandedBytes-expanded) {
				return nil, fmt.Errorf("CPIO expanded bytes exceed limit %d", limits.MaxExpandedBytes)
			}
			content, err := io.ReadAll(io.LimitReader(
				io.NewSectionReader(record.ReaderAt, 0, int64(record.FileSize)),
				int64(record.FileSize)+1,
			))
			if err != nil {
				return nil, fmt.Errorf("read CPIO entry %q: %w", record.Name, err)
			}
			if uint64(len(content)) != record.FileSize {
				return nil, fmt.Errorf("short CPIO entry %q: got %d bytes, want %d", record.Name, len(content), record.FileSize)
			}
			expanded += int64(record.FileSize)
			entry.MaterializedBytes = int64(record.FileSize)
			if entry.Kind == "symlink" {
				// Some vendor newc archives include a C-string terminator in
				// the declared symlink payload. It is framing, not part of the
				// filesystem target.
				entry.Linkname = strings.TrimSuffix(string(content), "\x00")
			} else {
				entry.Data = content
			}
		}
		files = append(files, entry)
	}
}

// preflightCPIO validates the complete record layout before u-root parses it.
// In particular, u-root allocates the declared name length before discovering
// a truncated archive and reports both an absent trailer and ordinary EOF as
// io.EOF. Keeping those checks here makes malformed firmware cheap to reject.
func preflightCPIO(data []byte, limits Limits) error {
	offset := 0
	entries := 0
	var expanded int64

	for {
		if offset == len(data) {
			return fmt.Errorf("CPIO archive is missing TRAILER!!!")
		}
		headerEnd, ok := boundedEnd(offset, newcHeaderSize, len(data))
		if !ok {
			return fmt.Errorf("truncated CPIO header at offset %d", offset)
		}
		header := data[offset:headerEnd]
		switch magic := string(header[:6]); magic {
		case "070701":
		case "070702":
			return fmt.Errorf("CPIO crc format 070702 is not supported")
		default:
			return fmt.Errorf("invalid CPIO magic %q at offset %d", magic, offset)
		}

		var fields [newcNumericFields]uint64
		for i := range fields {
			start := 6 + i*newcNumericHexSize
			value, err := strconv.ParseUint(string(header[start:start+newcNumericHexSize]), 16, 32)
			if err != nil {
				return fmt.Errorf("invalid CPIO header field %d at offset %d: %w", i, offset, err)
			}
			fields[i] = value
		}
		mode := fields[1]
		fileSize := fields[6]
		nameSize := fields[11]
		if nameSize == 0 {
			return fmt.Errorf("invalid CPIO name size at offset %d", offset)
		}
		if nameSize > maxCPIONameBytes {
			return fmt.Errorf("CPIO name size %d exceeds limit %d at offset %d", nameSize, maxCPIONameBytes, offset)
		}

		nameStart := headerEnd
		nameEnd, ok := boundedEnd(nameStart, nameSize, len(data))
		if !ok || data[nameEnd-1] != 0 {
			return fmt.Errorf("truncated CPIO name at offset %d", offset)
		}
		rawName := string(data[nameStart : nameEnd-1])
		if !safeArchivePath(rawName) {
			return fmt.Errorf("unsafe CPIO path %q", rawName)
		}
		fileStart, ok := alignedCPIOOffset(nameEnd, len(data))
		if !ok {
			return fmt.Errorf("truncated CPIO name padding at offset %d", offset)
		}
		fileEnd, ok := boundedEnd(fileStart, fileSize, len(data))
		if !ok {
			return fmt.Errorf("CPIO entry %q exceeds archive", rawName)
		}
		next, ok := alignedCPIOOffset(fileEnd, len(data))
		if !ok {
			return fmt.Errorf("truncated CPIO data padding for %q", rawName)
		}

		if rawName == cpio.Trailer {
			if fileSize != 0 {
				return fmt.Errorf("CPIO trailer has non-zero size %d", fileSize)
			}
			return nil
		}
		entries++
		if entries > limits.MaxArtifacts {
			return fmt.Errorf("CPIO entry count exceeds limit %d", limits.MaxArtifacts)
		}
		fileType := uint32(mode) & cpio.S_IFMT
		if fileType == cpio.S_IFREG || fileType == cpio.S_IFLNK {
			if fileSize > uint64(limits.MaxExpandedBytes-expanded) {
				return fmt.Errorf("CPIO expanded bytes exceed limit %d", limits.MaxExpandedBytes)
			}
			expanded += int64(fileSize)
		}
		offset = next
	}
}

func alignedCPIOOffset(value, total int) (int, bool) {
	padding := (4 - value%4) % 4
	if padding > total-value {
		return 0, false
	}
	return value + padding, true
}

func rawCPIOName(data []byte, recordOffset int64) (string, error) {
	if recordOffset < 0 || recordOffset+newcHeaderSize > int64(len(data)) {
		return "", fmt.Errorf("CPIO record offset %d exceeds input", recordOffset)
	}
	header := data[recordOffset : recordOffset+newcHeaderSize]
	nameSize, err := strconv.ParseUint(string(header[94:102]), 16, 32)
	if err != nil || nameSize == 0 {
		return "", fmt.Errorf("invalid CPIO name size at offset %d", recordOffset)
	}
	start := int(recordOffset) + newcHeaderSize
	end, ok := boundedEnd(start, nameSize, len(data))
	if !ok || data[end-1] != 0 {
		return "", fmt.Errorf("truncated CPIO name at offset %d", recordOffset)
	}
	return string(data[start : end-1]), nil
}

func boundedEnd(start int, size uint64, total int) (int, bool) {
	if start < 0 || start > total || size > uint64(total-start) {
		return 0, false
	}
	return start + int(size), true
}

func safeArchivePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return false
	}
	clean := path.Clean(name)
	return clean == name && clean != ".." && !strings.HasPrefix(clean, "../")
}

func align4(value int) int {
	return (value + 3) &^ 3
}
