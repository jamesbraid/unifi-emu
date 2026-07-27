package fwextract

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
func parseCPIO(data []byte, limits limits) ([]decodedFile, error) {
	return parseCPIOWithTrailingData(data, limits, false)
}

func parseEmbeddedCPIO(data []byte, limits limits) ([]decodedFile, error) {
	return parseCPIOWithTrailingData(data, limits, true)
}

func parseCPIOWithTrailingData(data []byte, limits limits, allowTrailingData bool) ([]decodedFile, error) {
	limits = limits.withDefaults()
	archives, err := preflightCPIOArchivesMode(data, limits, allowTrailingData)
	if err != nil {
		return nil, err
	}
	var files []decodedFile

	for _, archive := range archives {
		reader := cpio.Newc.Reader(bytes.NewReader(data[archive.Start:archive.End]))
		memberFiles := 0
		for {
			record, err := reader.ReadRecord()
			if err == io.EOF {
				if memberFiles != len(archive.Records) {
					return nil, fmt.Errorf(
						"CPIO parser returned %d records, preflight validated %d",
						memberFiles, len(archive.Records),
					)
				}
				break
			}
			if err != nil {
				return nil, fmt.Errorf("read CPIO record: %w", err)
			}
			if memberFiles >= len(archive.Records) {
				return nil, fmt.Errorf("CPIO parser returned an unvalidated record %q", record.Name)
			}
			validated := archive.Records[memberFiles]
			globalPosition := int64(archive.Start) + record.RecPos
			if globalPosition != validated.Offset || record.Name != validated.Name ||
				record.FileSize != validated.Size {
				return nil, fmt.Errorf(
					"CPIO record does not match preflight: got %q at %d size %d, want %q at %d size %d",
					record.Name, globalPosition, record.FileSize,
					validated.Name, validated.Offset, validated.Size,
				)
			}
			entry := decodedFile{
				Path: record.Name,
				Mode: int64(record.Mode & 0o7777),
			}
			switch record.Mode & cpio.S_IFMT {
			case cpio.S_IFDIR:
				entry.Kind = "directory"
			case cpio.S_IFREG:
				entry.Kind = "regular"
				if record.NLink > 1 {
					entry.LinkKey = fmt.Sprintf("cpio:%d:%d:%d", record.Major, record.Minor, record.Ino)
					if archive.Start != 0 {
						entry.LinkKey = fmt.Sprintf("cpio@%d:%d:%d:%d",
							archive.Start, record.Major, record.Minor, record.Ino)
					}
				}
			case cpio.S_IFLNK:
				entry.Kind = "symlink"
			case cpio.S_IFCHR:
				entry.Kind = "char-device"
				entry.DeviceMajor, entry.DeviceMinor = int64(record.Rmajor), int64(record.Rminor)
			case cpio.S_IFBLK:
				entry.Kind = "block-device"
				entry.DeviceMajor, entry.DeviceMinor = int64(record.Rmajor), int64(record.Rminor)
			case cpio.S_IFIFO:
				entry.Kind = "fifo"
			case cpio.S_IFSOCK:
				return nil, fmt.Errorf("unsupported CPIO socket %q", record.Name)
			default:
				return nil, fmt.Errorf("unsupported CPIO entry mode %#o for %q", record.Mode&cpio.S_IFMT, record.Name)
			}
			if entry.Kind == "regular" || entry.Kind == "symlink" {
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
			memberFiles++
		}
	}
	return files, nil
}

type cpioRecordDescriptor struct {
	Name   string
	Offset int64
	Size   uint64
}

type cpioArchiveDescriptor struct {
	Start   int
	End     int
	Records []cpioRecordDescriptor
}

// preflightCPIO validates the complete record layout before u-root parses it.
// In particular, u-root allocates the declared name length before discovering
// a truncated archive and reports both an absent trailer and ordinary EOF as
// io.EOF. Keeping those checks here makes malformed firmware cheap to reject.
func preflightCPIO(data []byte, limits limits) ([]cpioRecordDescriptor, error) {
	archives, err := preflightCPIOArchives(data, limits)
	if err != nil {
		return nil, err
	}
	var records []cpioRecordDescriptor
	for _, archive := range archives {
		records = append(records, archive.Records...)
	}
	return records, nil
}

func preflightCPIOArchives(data []byte, limits limits) ([]cpioArchiveDescriptor, error) {
	return preflightCPIOArchivesMode(data, limits, false)
}

func preflightCPIOArchivesMode(data []byte, limits limits, allowTrailingData bool) ([]cpioArchiveDescriptor, error) {
	offset := 0
	archiveStart := 0
	var archives []cpioArchiveDescriptor
	var memberRecords []cpioRecordDescriptor
	var expanded int64
	totalRecords := 0

	for {
		if offset == len(data) {
			return nil, fmt.Errorf("cpio archive is missing trailer record")
		}
		headerEnd, ok := boundedEnd(offset, newcHeaderSize, len(data))
		if !ok {
			return nil, fmt.Errorf("truncated CPIO header at offset %d", offset)
		}
		header := data[offset:headerEnd]
		switch magic := string(header[:6]); magic {
		case "070701":
		case "070702":
			return nil, fmt.Errorf("CPIO crc format 070702 is not supported")
		default:
			return nil, fmt.Errorf("invalid CPIO magic %q at offset %d", magic, offset)
		}

		var fields [newcNumericFields]uint64
		for i := range fields {
			start := 6 + i*newcNumericHexSize
			value, err := strconv.ParseUint(string(header[start:start+newcNumericHexSize]), 16, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid CPIO header field %d at offset %d: %w", i, offset, err)
			}
			fields[i] = value
		}
		mode := fields[1]
		fileSize := fields[6]
		nameSize := fields[11]
		if nameSize == 0 {
			return nil, fmt.Errorf("invalid CPIO name size at offset %d", offset)
		}
		if nameSize > maxCPIONameBytes {
			return nil, fmt.Errorf("CPIO name size %d exceeds limit %d at offset %d", nameSize, maxCPIONameBytes, offset)
		}

		nameStart := headerEnd
		nameEnd, ok := boundedEnd(nameStart, nameSize, len(data))
		if !ok || data[nameEnd-1] != 0 {
			return nil, fmt.Errorf("truncated CPIO name at offset %d", offset)
		}
		rawName := string(data[nameStart : nameEnd-1])
		if !safeArchivePath(rawName) {
			return nil, fmt.Errorf("unsafe CPIO path %q", rawName)
		}
		fileStart, ok := alignedCPIOOffset(nameEnd, len(data))
		if !ok {
			return nil, fmt.Errorf("truncated CPIO name padding at offset %d", offset)
		}
		fileEnd, ok := boundedEnd(fileStart, fileSize, len(data))
		if !ok {
			return nil, fmt.Errorf("CPIO entry %q exceeds archive", rawName)
		}
		next, ok := alignedCPIOOffset(fileEnd, len(data))
		if !ok {
			return nil, fmt.Errorf("truncated CPIO data padding for %q", rawName)
		}

		if rawName == cpio.Trailer {
			if fileSize != 0 {
				return nil, fmt.Errorf("CPIO trailer has non-zero size %d", fileSize)
			}
			archives = append(archives, cpioArchiveDescriptor{
				Start: archiveStart, End: next, Records: memberRecords,
			})
			offset = next
			for offset < len(data) && data[offset] == 0 {
				offset++
			}
			if offset == len(data) {
				return archives, nil
			}
			if len(data)-offset < 6 ||
				(string(data[offset:offset+6]) != "070701" &&
					string(data[offset:offset+6]) != "070702") {
				if allowTrailingData {
					return archives, nil
				}
				return nil, fmt.Errorf("non-NUL data after CPIO trailer at offset %d", offset)
			}
			archiveStart = offset
			memberRecords = nil
			continue
		}
		if totalRecords >= limits.MaxArtifacts {
			return nil, fmt.Errorf("CPIO entry count exceeds limit %d", limits.MaxArtifacts)
		}
		fileType := uint32(mode) & cpio.S_IFMT
		if fileType == cpio.S_IFREG || fileType == cpio.S_IFLNK {
			if fileSize > uint64(limits.MaxExpandedBytes-expanded) {
				return nil, fmt.Errorf("CPIO expanded bytes exceed limit %d", limits.MaxExpandedBytes)
			}
			expanded += int64(fileSize)
		}
		memberRecords = append(memberRecords, cpioRecordDescriptor{
			Name: rawName, Offset: int64(offset), Size: fileSize,
		})
		totalRecords++
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
