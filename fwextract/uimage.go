package fwextract

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

const uImageMagic = 0x27051956

func parseUImage(data []byte) (containerChild, error) {
	if len(data) < 64 || binary.BigEndian.Uint32(data[:4]) != uImageMagic {
		return containerChild{}, fmt.Errorf("not a legacy uImage")
	}
	header := append([]byte(nil), data[:64]...)
	wantHeaderCRC := binary.BigEndian.Uint32(header[4:8])
	binary.BigEndian.PutUint32(header[4:8], 0)
	if got := crc32.ChecksumIEEE(header); got != wantHeaderCRC {
		return containerChild{}, fmt.Errorf("uImage header CRC mismatch: got %08x, want %08x", got, wantHeaderCRC)
	}
	size := binary.BigEndian.Uint32(data[12:16])
	end, ok := boundedEnd(64, uint64(size), len(data))
	if !ok {
		return containerChild{}, fmt.Errorf("uImage payload exceeds image")
	}
	payload := data[64:end]
	wantDataCRC := binary.BigEndian.Uint32(data[24:28])
	if got := crc32.ChecksumIEEE(payload); got != wantDataCRC {
		return containerChild{}, fmt.Errorf("uImage data CRC mismatch: got %08x, want %08x", got, wantDataCRC)
	}
	compressions := map[byte]string{
		0: "none", 1: "gzip", 2: "bzip2", 3: "lzma",
		4: "lzo", 5: "lz4", 6: "zstd",
	}
	compression := compressions[data[31]]
	if compression == "" {
		compression = fmt.Sprintf("unknown-%d", data[31])
	}
	return containerChild{
		Name: cString(data[32:64]), Data: payload, Offset: 64, Compression: compression,
	}, nil
}
