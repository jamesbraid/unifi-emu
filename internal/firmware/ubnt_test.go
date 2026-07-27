package firmware

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
)

func TestParseUBNTPartsAndEMMCRecords(t *testing.T) {
	part := ubntPart("kernel", []byte("legacy"))
	emmc := ubntEMMC("kernel0", []byte("modern"))
	image := ubntImage(append(part, emmc...))

	children, err := parseUBNT(image, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2", len(children))
	}
	if children[0].Name != "kernel" || string(children[0].Data) != "legacy" ||
		children[0].Offset != 268+56 {
		t.Fatalf("PART = %+v", children[0])
	}
	if children[1].Name != "kernel0" || string(children[1].Data) != "modern" ||
		children[1].Offset != int64(268+len(part)+56) {
		t.Fatalf("EMMC = %+v", children[1])
	}
}

func TestParseUBNTRejectsTruncatedPart(t *testing.T) {
	image := ubntImage(ubntPart("kernel", []byte("payload")))
	binary.BigEndian.PutUint32(image[268+48:268+52], 0xffffffff)
	if _, err := parseUBNT(image, DefaultLimits()); err == nil {
		t.Fatal("accepted out-of-bounds PART")
	}
}

func TestParseUBNTRejectsTruncatedEndRecord(t *testing.T) {
	image := ubntImage(nil)
	image = image[:len(image)-1]
	if _, err := parseUBNT(image, DefaultLimits()); err == nil {
		t.Fatal("accepted truncated ENDS record")
	}
}

func ubntImage(records []byte) []byte {
	header := make([]byte, 268)
	copy(header, "UBNTtest")
	binary.BigEndian.PutUint32(header[260:264], crc32.ChecksumIEEE(header[:260]))
	out := append(header, records...)
	out = append(out, []byte("ENDS")...)
	return append(out, make([]byte, 260)...)
}

func ubntPart(name string, data []byte) []byte {
	header := make([]byte, 56)
	copy(header, "PART")
	copy(header[4:20], name)
	binary.BigEndian.PutUint32(header[48:52], uint32(len(data)))
	binary.BigEndian.PutUint32(header[52:56], uint32(len(data)))
	out := append(header, data...)
	trailer := make([]byte, 8)
	binary.BigEndian.PutUint32(trailer[:4], crc32.ChecksumIEEE(out))
	return append(out, trailer...)
}

func ubntEMMC(name string, data []byte) []byte {
	header := make([]byte, 56)
	copy(header, "EMMC")
	copy(header[4:36], name)
	binary.BigEndian.PutUint32(header[48:52], uint32(len(data)))
	out := append(header, data...)
	trailer := make([]byte, 8)
	binary.BigEndian.PutUint32(trailer[:4], crc32.ChecksumIEEE(out))
	return append(out, trailer...)
}
