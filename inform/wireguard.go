package inform

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
)

// wireGuardKeypair returns a fresh Curve25519 keypair in WireGuard's form:
// a clamped 32-byte private key and its public key, both base64.
func wireGuardKeypair() (private, public string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	raw[0] &= 248
	raw[31] = raw[31]&127 | 64
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", "", err
	}
	enc := base64.StdEncoding
	return enc.EncodeToString(raw), enc.EncodeToString(key.PublicKey().Bytes()), nil
}
