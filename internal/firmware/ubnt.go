package firmware

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strings"
)

type containerChild struct {
	Name        string
	Data        []byte
	Offset      int64
	Compression string
}

func parseUBNT(data []byte, limits Limits) ([]containerChild, error) {
	limits = limits.withDefaults()
	if len(data) < 268 || (!bytes.Equal(data[:4], []byte("UBNT")) && !bytes.Equal(data[:4], []byte("OPEN"))) {
		return nil, fmt.Errorf("not a UBNT firmware container")
	}
	if got, want := crc32.ChecksumIEEE(data[:260]), binary.BigEndian.Uint32(data[260:264]); got != want {
		return nil, fmt.Errorf("UBNT header CRC mismatch: got %08x, want %08x", got, want)
	}
	var children []containerChild
	offset := 268
	for {
		if len(data)-offset < 4 {
			return nil, fmt.Errorf("truncated UBNT record at offset %d", offset)
		}
		if len(children) >= limits.MaxArtifacts {
			return nil, fmt.Errorf("UBNT child count exceeds limit %d", limits.MaxArtifacts)
		}
		switch string(data[offset : offset+4]) {
		case "END.", "ENDS":
			// The end record is a four-byte marker followed by a 256-byte
			// signature and a four-byte reserved field.
			if len(data)-offset != 264 {
				return nil, fmt.Errorf("invalid UBNT end record size %d at offset %d", len(data)-offset, offset)
			}
			return children, nil
		case "PART":
			if len(data)-offset < 56 {
				return nil, fmt.Errorf("truncated PART header at offset %d", offset)
			}
			size := binary.BigEndian.Uint32(data[offset+48 : offset+52])
			payloadStart := offset + 56
			payloadEnd, ok := boundedEnd(payloadStart, uint64(size), len(data))
			if !ok || len(data)-payloadEnd < 8 {
				return nil, fmt.Errorf("PART %q exceeds container", cString(data[offset+4:offset+20]))
			}
			want := binary.BigEndian.Uint32(data[payloadEnd : payloadEnd+4])
			if got := crc32.ChecksumIEEE(data[offset:payloadEnd]); got != want {
				return nil, fmt.Errorf("PART %q CRC mismatch: got %08x, want %08x",
					cString(data[offset+4:offset+20]), got, want)
			}
			children = append(children, containerChild{
				Name: cString(data[offset+4 : offset+20]), Data: data[payloadStart:payloadEnd],
				Offset: int64(payloadStart),
			})
			offset = payloadEnd + 8
		case "EMMC":
			if len(data)-offset < 56 {
				return nil, fmt.Errorf("truncated EMMC header at offset %d", offset)
			}
			size := binary.BigEndian.Uint32(data[offset+48 : offset+52])
			payloadStart := offset + 56
			payloadEnd, ok := boundedEnd(payloadStart, uint64(size), len(data))
			if !ok || len(data)-payloadEnd < 8 {
				return nil, fmt.Errorf("EMMC %q exceeds container", cString(data[offset+4:offset+36]))
			}
			want := binary.BigEndian.Uint32(data[payloadEnd : payloadEnd+4])
			if got := crc32.ChecksumIEEE(data[offset:payloadEnd]); got != want {
				return nil, fmt.Errorf("EMMC %q CRC mismatch: got %08x, want %08x",
					cString(data[offset+4:offset+36]), got, want)
			}
			children = append(children, containerChild{
				Name: cString(data[offset+4 : offset+36]), Data: data[payloadStart:payloadEnd],
				Offset: int64(payloadStart),
			})
			offset = payloadEnd + 8
		default:
			return nil, fmt.Errorf("unsupported UBNT record %q at offset %d", data[offset:offset+4], offset)
		}
	}
}

func cString(value []byte) string {
	if i := bytes.IndexByte(value, 0); i >= 0 {
		value = value[:i]
	}
	return strings.TrimSpace(string(value))
}
