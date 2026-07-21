// Package inform implements the UniFi inform wire protocol: the TNBU
// binary packet, AES-128-CBC/GCM encryption, and zlib/snappy compression.
// Construction matches amd989/unifi-gateway unifi_protocol.py (MIT);
// verified against a real controller by the 2026-07-19 spike.
package inform

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/golang/snappy"
)

const (
	flagEncrypted = 1 << 0
	flagZlib      = 1 << 1
	flagSnappy    = 1 << 2
	flagGCM       = 1 << 3

	packetVersion  = 1
	payloadVersion = 1
	headerLen      = 40
)

// Packet is one decoded inform message (request or response).
type Packet struct {
	MAC     [6]byte
	Payload []byte // plaintext JSON
}

// Encode serializes, zlib-compresses and CBC-encrypts p with keyHex —
// what amd989 sends and what the spike proved a real controller accepts.
func (p *Packet) Encode(keyHex string) ([]byte, error) {
	key, err := ParseKey(keyHex)
	if err != nil {
		return nil, err
	}
	iv := randomIV()

	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(p.Payload); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	body, err := encryptCBC(key, iv, buf.Bytes())
	if err != nil {
		return nil, err
	}

	head := make([]byte, 0, headerLen)
	head = append(head, "TNBU"...)
	head = binary.BigEndian.AppendUint32(head, packetVersion)
	head = append(head, p.MAC[:]...)
	head = binary.BigEndian.AppendUint16(head, flagEncrypted|flagZlib)
	head = append(head, iv...)
	head = binary.BigEndian.AppendUint32(head, payloadVersion)
	head = binary.BigEndian.AppendUint32(head, uint32(len(body)))
	return append(head, body...), nil
}

// Decode parses, decrypts and decompresses one packet with keyHex.
func Decode(data []byte, keyHex string) (*Packet, error) {
	if len(data) < headerLen {
		return nil, fmt.Errorf("packet too short: %d bytes", len(data))
	}
	if string(data[:4]) != "TNBU" {
		return nil, fmt.Errorf("bad magic %q", data[:4])
	}
	key, err := ParseKey(keyHex)
	if err != nil {
		return nil, err
	}
	p := &Packet{}
	copy(p.MAC[:], data[8:14])
	flags := binary.BigEndian.Uint16(data[14:16])
	iv := data[16:32]
	n := int(binary.BigEndian.Uint32(data[36:40]))
	// n < 0 catches the uint32->int wrap on 32-bit platforms; comparing
	// against len(data)-headerLen avoids overflowing headerLen+n.
	if n < 0 || n > len(data)-headerLen {
		return nil, fmt.Errorf("payload length %d exceeds packet (%d bytes)", n, len(data))
	}
	body := data[headerLen : headerLen+n]

	var plain []byte
	switch {
	case flags&flagEncrypted == 0:
		plain = body
	case flags&flagGCM != 0:
		// AAD is the full 40-byte header both ways. amd989's encode
		// authenticates only the first 36 bytes, contradicting its own
		// decode (40); 40 is self-consistent and matches what we emit.
		if plain, err = decryptGCM(key, iv, data[:headerLen], body); err != nil {
			return nil, fmt.Errorf("gcm: %w", err)
		}
	default:
		if plain, err = decryptCBC(key, iv, body); err != nil {
			return nil, fmt.Errorf("cbc: %w", err)
		}
	}

	switch {
	case flags&flagSnappy != 0:
		if plain, err = snappy.Decode(nil, plain); err != nil {
			return nil, fmt.Errorf("snappy: %w", err)
		}
	case flags&flagZlib != 0:
		zr, err := zlib.NewReader(bytes.NewReader(plain))
		if err != nil {
			return nil, fmt.Errorf("zlib: %w", err)
		}
		defer zr.Close()
		if plain, err = io.ReadAll(zr); err != nil {
			return nil, fmt.Errorf("zlib: %w", err)
		}
	}
	p.Payload = plain
	return p, nil
}
