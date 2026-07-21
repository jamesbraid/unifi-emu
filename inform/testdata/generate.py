# /// script
# requires-python = ">=3.9"
# dependencies = ["pycryptodome", "python-snappy"]
# ///
"""Generate inform-packet test vectors with the same construction as
amd989/unifi-gateway unifi_protocol.py (fixed IV for determinism)."""
import json, struct, zlib, snappy
from Crypto.Cipher import AES
from binascii import a2b_hex

KEY = "ba86f2bbe107c7c57eb5f2690775c712"
MAC = bytes([0x00, 0x27, 0x22, 0xE0, 0x00, 0x01])
IV = bytes(range(16))
# Compact separators: the Go tests assert on the exact substrings
# '"_type":"noop"' and '"interval":5'.
PAYLOAD = json.dumps({"_type": "noop", "interval": 5}, separators=(",", ":")).encode()

def packet(flags, payload):
    return (b"TNBU" + struct.pack(">I", 1) + MAC + struct.pack(">H", flags)
            + IV + struct.pack(">I", 1) + struct.pack(">I", len(payload)) + payload)

def cbc(data):
    pad = 16 - len(data) % 16
    data = data + bytes([pad]) * pad
    return AES.new(a2b_hex(KEY), AES.MODE_CBC, IV).encrypt(data)

vectors = {
    "cbc_zlib":   (0x01 | 0x02, cbc(zlib.compress(PAYLOAD))),
    "cbc_snappy": (0x01 | 0x04, cbc(snappy.compress(PAYLOAD))),
    "cbc_plain":  (0x01,        cbc(PAYLOAD)),
}
for name, (flags, body) in vectors.items():
    with open(f"inform/testdata/{name}.hex", "w") as f:
        f.write(packet(flags, body).hex() + "\n")
print("wrote", ", ".join(vectors))
