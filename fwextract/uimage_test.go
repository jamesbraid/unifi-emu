package fwextract

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
)

func TestParseUImageReturnsPayloadMetadata(t *testing.T) {
	image := testUImage("kernel", 3, []byte("payload"))
	child, err := parseUImage(image)
	if err != nil {
		t.Fatal(err)
	}
	if child.Name != "kernel" || child.Compression != "lzma" ||
		child.Offset != 64 || string(child.Data) != "payload" {
		t.Fatalf("child = %+v", child)
	}
}

func TestParseUImageRejectsBadDataCRC(t *testing.T) {
	image := testUImage("kernel", 0, []byte("payload"))
	image[len(image)-1] ^= 0xff
	if _, err := parseUImage(image); err == nil {
		t.Fatal("accepted corrupt uImage data")
	}
}

func testUImage(name string, compression byte, data []byte) []byte {
	header := make([]byte, 64)
	binary.BigEndian.PutUint32(header[0:4], 0x27051956)
	binary.BigEndian.PutUint32(header[12:16], uint32(len(data)))
	binary.BigEndian.PutUint32(header[24:28], crc32.ChecksumIEEE(data))
	header[31] = compression
	copy(header[32:64], name)
	check := append([]byte(nil), header...)
	binary.BigEndian.PutUint32(check[4:8], 0)
	binary.BigEndian.PutUint32(header[4:8], crc32.ChecksumIEEE(check))
	return append(header, data...)
}
