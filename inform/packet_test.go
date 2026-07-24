package inform

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testMAC = [6]byte{0x00, 0x27, 0x22, 0xE0, 0x00, 0x01}

func readVector(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".hex"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeCrossImplementation(t *testing.T) {
	for _, name := range []string{"cbc_zlib", "cbc_snappy", "cbc_plain"} {
		t.Run(name, func(t *testing.T) {
			pkt := readVector(t, name)
			p, err := Decode(pkt, DefaultKey)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if p.MAC != testMAC {
				t.Fatalf("MAC = %x", p.MAC)
			}
			got := string(p.Payload)
			if !strings.Contains(got, `"_type":"noop"`) || !strings.Contains(got, `"interval":5`) {
				t.Fatalf("payload = %q", got)
			}
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := &Packet{MAC: testMAC, Payload: []byte(`{"state":1,"default":true}`)}
	pkt, err := in.Encode(DefaultKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(pkt[:4]) != "TNBU" {
		t.Fatalf("magic = %q", pkt[:4])
	}
	out, err := Decode(pkt, DefaultKey)
	if err != nil {
		t.Fatal(err)
	}
	if out.MAC != in.MAC || string(out.Payload) != string(in.Payload) {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestEncodeGCMRoundTrip(t *testing.T) {
	in := &Packet{MAC: testMAC, Payload: []byte(`{"state":4,"default":false}`)}
	pkt, err := in.EncodeGCM(DefaultKey)
	if err != nil {
		t.Fatal(err)
	}
	flags := binary.BigEndian.Uint16(pkt[14:16])
	if flags&flagGCM == 0 {
		t.Fatalf("flags = %#x, want GCM bit", flags)
	}
	out, err := Decode(pkt, DefaultKey)
	if err != nil {
		t.Fatal(err)
	}
	if out.MAC != in.MAC || string(out.Payload) != string(in.Payload) {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestDecodeErrors(t *testing.T) {
	good, _ := (&Packet{MAC: testMAC, Payload: []byte("{}")}).Encode(DefaultKey)

	if _, err := Decode([]byte("short"), DefaultKey); err == nil {
		t.Fatal("expected error for short packet")
	}
	bad := append([]byte(nil), good...)
	bad[0] = 'X'
	if _, err := Decode(bad, DefaultKey); err == nil {
		t.Fatal("expected error for bad magic")
	}
	if _, err := Decode(good[:len(good)-4], DefaultKey); err == nil {
		t.Fatal("expected error for truncated payload")
	}
	if _, err := Decode(good, "00112233445566778899aabbccddeeff"); err == nil {
		t.Fatal("expected error for wrong key")
	}
	oversized := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(oversized[36:40], 0xFFFFFFFF)
	if _, err := Decode(oversized, DefaultKey); err == nil {
		t.Fatal("expected error for oversized payload length")
	}
}

// Pins the GCM decode path: AAD must be the full 40-byte header,
// length field included. A regression to amd989's 36-byte encode-side
// AAD fails this test loudly.
func TestDecodeGCM(t *testing.T) {
	key, _ := ParseKey(DefaultKey)
	iv := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	plain := []byte(`{"_type":"noop"}`)

	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	comp := buf.Bytes()

	head := make([]byte, 0, headerLen)
	head = append(head, "TNBU"...)
	head = binary.BigEndian.AppendUint32(head, packetVersion)
	head = append(head, testMAC[:]...)
	head = binary.BigEndian.AppendUint16(head, flagEncrypted|flagGCM|flagZlib)
	head = append(head, iv...)
	head = binary.BigEndian.AppendUint32(head, payloadVersion)
	head = binary.BigEndian.AppendUint32(head, uint32(len(comp)+16))
	sealed, err := encryptGCM(key, iv, head, comp)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Decode(append(head, sealed...), DefaultKey)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if p.MAC != testMAC || string(p.Payload) != string(plain) {
		t.Fatalf("got MAC %x payload %q", p.MAC, p.Payload)
	}
}
