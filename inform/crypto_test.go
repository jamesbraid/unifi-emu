package inform

import (
	"encoding/hex"
	"testing"
)

func TestAESCBCRoundTrip(t *testing.T) {
	key, _ := ParseKey(DefaultKey)
	iv := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	plain := []byte(`{"hello":"unifi"}`)
	ct, err := encryptCBC(key, iv, plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(ct)%16 != 0 {
		t.Fatalf("ciphertext len %d not block-aligned", len(ct))
	}
	got, err := decryptCBC(key, iv, ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Fatalf("got %q want %q", got, plain)
	}
}

// NIST SP 800-38A F.2.1 AES-128-CBC known answer (first block).
func TestAESCBCKnownVector(t *testing.T) {
	key, _ := ParseKey("2b7e151628aed2a6abf7158809cf4f3c")
	iv := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f}
	plain := []byte{0x6b, 0xc1, 0xbe, 0xe2, 0x2e, 0x40, 0x9f, 0x96,
		0xe9, 0x3d, 0x7e, 0x11, 0x73, 0x93, 0x17, 0x2a}
	ct, err := encryptCBC(key, iv, plain)
	if err != nil {
		t.Fatal(err)
	}
	if want := "7649abac8119b246cee98e9b12e9197d"; hex.EncodeToString(ct[:16]) != want {
		t.Fatalf("first block = %x, want %s", ct[:16], want)
	}
}

func TestDecryptCBCRejectsBadPadding(t *testing.T) {
	key, _ := ParseKey(DefaultKey)
	iv := make([]byte, 16)
	ct, _ := encryptCBC(key, iv, []byte("0123456789abcdef0123456789abcdef"))
	ct[20] ^= 0xff // block 2 of 3; CBC chaining flips a pad byte in the final plaintext block
	if _, err := decryptCBC(key, iv, ct); err == nil {
		t.Fatal("expected padding error")
	}
}

func TestAESGCMRoundTrip(t *testing.T) {
	key, _ := ParseKey(DefaultKey)
	iv := []byte("0123456789abcdef")
	aad := []byte("TNBU-header-40-bytes-aaaaaaaaaaaaaaaaaaaaa")
	plain := []byte(`{"_type":"noop"}`)
	ct, err := encryptGCM(key, iv, aad, plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decryptGCM(key, iv, aad, ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Fatalf("got %q want %q", got, plain)
	}
	ct[0] ^= 1
	if _, err := decryptGCM(key, iv, aad, ct); err == nil {
		t.Fatal("expected GCM auth error")
	}
}

func TestParseKey(t *testing.T) {
	k, err := ParseKey(DefaultKey)
	if err != nil || len(k) != 16 {
		t.Fatalf("ParseKey(DefaultKey) = %v len %d, err %v", k, len(k), err)
	}
	if _, err := ParseKey("zz"); err == nil {
		t.Fatal("expected error for bad hex")
	}
	if _, err := ParseKey("0011"); err == nil {
		t.Fatal("expected error for wrong length")
	}
}
