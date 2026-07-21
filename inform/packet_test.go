package inform

import (
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
}
