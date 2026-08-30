# UniFi device protocol — wire-level spec

A UniFi device speaks two wire protocols: **L2 discovery**, a UDP broadcast
that lets a controller find a device on the local network segment, and **L3
inform**, the HTTP heartbeat that carries adoption and ongoing management. This
document specifies both, for anyone implementing the device side in firmware —
the motivating case is UniFi support in third-party MoCA-adapter firmware
written in C.

The two protocols are independent. Adoption runs entirely over inform and never
depends on discovery having fired: a device pointed at a controller's inform URL
adopts normally without broadcasting anything. Discovery only helps a controller
populate its "Devices" list for hardware on its own segment. **Implement inform
first. Add discovery only if the firmware needs to appear in a controller's
live-scan UI.**

The reference implementation is this repository's Go code, and it is the
authority where this document and the code disagree:

- `inform/packet.go`, `inform/crypto.go` — the inform wire packet and its crypto.
- `inform/session.go`, `inform/tables.go` — the inform payload and adoption state machine.
- `discovery/packet.go` — the L2 discovery packet.

## Inform wire packet

Both directions use the same packet, named `TNBU` after its 4-byte magic. A
device POSTs one encoded packet as the HTTP body to the controller's inform URL
(commonly `http://<controller>:8080/inform`) with `Content-Type:
application/x-binary`. On HTTP 200 the response body is another TNBU packet,
decrypted with whatever key the device currently holds. An unadopted device
often gets HTTP 404 instead — that is benign, covered under the adoption
handshake below.

### Header (40 bytes)

| Offset | Size | Field | Notes |
|---:|---:|---|---|
| 0 | 4 | Magic | ASCII `TNBU` |
| 4 | 4 | Packet version | uint32 big-endian, always `1` |
| 8 | 6 | Device MAC | raw bytes, not text |
| 14 | 2 | Flags | uint16 big-endian, see below |
| 16 | 16 | IV | random per packet, also the AES-GCM nonce when the GCM flag is set |
| 32 | 4 | Payload version | uint32 big-endian, always `1` |
| 36 | 4 | Body length | uint32 big-endian, byte length of what follows (ciphertext, or ciphertext‖tag for GCM) |
| 40 | body length | Body | the encrypted, compressed payload |

Both version fields sit on the wire but carry no dispatch logic. A decoder reads
the flags byte and the fixed offsets above. To reject a future incompatible
version, add that check yourself.

### Flags

| Bit | Mask | Meaning |
|---:|---:|---|
| 0 | 0x01 | Body is encrypted |
| 1 | 0x02 | Body is zlib-compressed (before encryption) |
| 2 | 0x04 | Body is snappy-compressed (before encryption) |
| 3 | 0x08 | Encryption is AES-GCM, not AES-128-CBC |

The reference implementation always sets the encrypted bit and always compresses
with zlib. It never emits the snappy flag and only decodes it, because some
UniFi firmware in the field uses snappy. A device that only needs to talk to a
controller can skip snappy and implement zlib alone.

### Crypto

Both modes derive a 16-byte AES key from a 32-hex-character authkey by plain hex
decode, no KDF. The key an unadopted device holds, `DefaultKey`, is
`MD5("ubnt")` = `ba86f2bbe107c7c57eb5f2690775c712`.

#### AES-128-CBC

PKCS#7 padding to the 16-byte block size. The IV is the 16 bytes in the header,
fresh per packet, never derived from anything else. To decrypt, CBC-decrypt and
then validate and strip the pad — reject if the pad byte is 0, exceeds the block
size, or the trailing bytes don't all match it.

#### AES-GCM

The nonce is **16 bytes**, not the 12 bytes most AEAD libraries default to. Build
the GCM context with an explicit 16-byte nonce length (Go:
`cipher.NewGCMWithNonceSize(block, 16)`, OpenSSL: `EVP_CTRL_GCM_SET_IVLEN` set to
16), or decryption fails with no useful error. The additional authenticated data
is the complete 40-byte header. Assemble the whole header, including the
body-length field, before computing the tag. The 16-byte tag is appended to the
ciphertext (`ciphertext‖tag`), not carried separately.

A device starts on AES-128-CBC. The controller switches it to AES-GCM by sending
`mgmt_cfg.use_aes_gcm=true` in a `setparam` reply (see the adoption handshake).
From the next inform on, the device encodes with GCM. There is no path back to
CBC.

zlib is the RFC 1950 stream, applied to the plaintext JSON payload before
encryption in both modes.

## Inform payload

The payload is a flat JSON object. Its shape depends on whether the device is
adopted, and on device type once adopted.

### Every inform, pending or adopted

`mac`, `serial`, `model`, `model_display`, `version` (firmware), `ip`,
`hostname`, `inform_url`, `uptime` (seconds since boot), `time` (Unix epoch),
`cfgversion`, `x_authkey` (the current key, echoed back), `default` and
`_default_key` (both `true` until adopted), `state` (`1` while pending),
`fw_caps`, `isolated`, `locating`, `selfrun_beacon`.

A device that runs the newer UDAPI config plane also sends `udapi_version` (an
object, `{"version": "<schema version>"}`) and `udapi_caps` (an int bitmap) on
every inform, adopted or not. **The two must go out together, never one without
the other.** A device on firmware 4.1.0 or newer that sends `udapi_caps` with no
`udapi_version` has its entire capability update dropped, storing none of
`fw_caps`, `hw_caps`, `switch_caps`, or `udapi_caps`, and ends up looking *less*
capable than a device that claimed nothing. Whether a device sends UDAPI fields
is a per-model fact, not a per-type rule. Claim only what the device can
actually service: the controller offers any claimed feature against the device,
and a claim it can't fulfil is worse than claiming nothing.

### Added once adopted

`state` becomes `4`. This is not the controller's REST `stat/device.state` — two
unrelated fields both named "state." Also added: `bootrom_version` and a
`sys_stats` object (`cpu`, `mem_total`, `mem_used`, `mem_buffer`), which is
distinct from the per-gateway `system-stats` object below. Both objects go on
the wire.

Per device type, adopted only:

- **Gateway** (`ugw`/`uxg`): `system-stats` (`cpu`, `mem`, `uptime`, all
  strings), `config_network_wan` (`{"type": "dhcp"}`), `netmask`, and `uplink`
  (`name`, `num_port`, `ip`, `mac`, `type`, `up`, `speed`, `max_speed`,
  `full_duplex`, `rx_bytes`, `tx_bytes`).
- **Switch** (`usw`): `port_table`, one entry per port (`ifname`, `name`,
  `port_idx`, `media`, `poe_caps`, `is_uplink`, `up`, `speed`, `full_duplex`,
  `rx_bytes`, `tx_bytes`), and `ethernet_table`, a single entry (`mac`, `name`,
  `num_port`).
- **AP** (`uap`) sends three radio tables plus its own wired-port tables:
    - `radio_table`, one entry per radio: `name`, `radio`, `channel`, `ht`,
      `min_txpower`, `max_txpower`, `nss`, `tx_power`, `radio_caps`,
      `antenna_gain`, `builtin_antenna`, `builtin_ant_gain`.
    - `radio_table_stats`, one entry per radio: `name`, `channel`, `tx_power`,
      `cu_self_tx`, `cu_self_rx`, `cu_total`, `num_sta`, `noise`.
    - `vap_table`, empty unless the device has configured SSIDs, one entry per
      radio-and-SSID pair: `essid`, `bssid`, `name`, `radio`, `up`, `channel`,
      `tx_power`, `num_sta`, `usage`, `id`, `ccq`, `rx_bytes`, `tx_bytes`,
      `rx_packets`, `tx_packets`, `sta_table`.
    - the same `ethernet_table` and `port_table` a switch sends, for the AP's
      own wired ports.

### Config the controller pushes back

The controller provisions config through a `setstate` reply carrying
`radio_table`, `vap_table`, `port_table`, or `port_overrides`. A device stashes
these and echoes them back verbatim on every later inform, overriding whatever
it would otherwise compute. A device that takes provisioned config and then
reports its own defaults looks, from the controller's side, exactly like one
that rejected the push.

## Adoption handshake

### Sequence

1. **Pending.** The device informs continuously — every 5 to 10 seconds is what
   controllers expect — using `DefaultKey`, with `state=1, default=true`. The
   controller lists it as pending. An unadopted device commonly gets **HTTP
   404** back. That is benign and expected: it means nothing is queued for this
   device, not an error, and a 404 carries no body while every other reply is a
   TNBU packet. Keep informing through the 404s.
2. An operator, or an API call, adopts the device on the controller.
3. On a later inform, the controller delivers a new authkey through one of the
   two channels below. The device adopts the new key, updates its `inform_url`
   if the controller sent a new one, and sets `adopted=true`, which flips its
   next payload to the adopted shape.
4. The controller keeps pushing `setparam` (`mgmt_cfg`) and `setstate` (config
   tables) replies as informs continue. The device is connected once a reply
   arrives to an inform that was already sent adopted.

### The two key-delivery channels

- **`set-adopt`** — a `cmd` reply, `{"_type": "cmd", "cmd": "set-adopt", "key":
  "<new authkey>", "uri": "<new inform URL, optional>"}`. Authoritative and
  unconditional: apply the key and URI whatever key the device currently holds.
- **`mgmt_cfg.authkey`** — a `setparam` reply whose `mgmt_cfg` field is a single
  string of newline-separated `key=value` pairs, not JSON, one of which may be
  `authkey=<new key>`. Other lines carry `cfgversion` and `use_aes_gcm`.

Controller builds differ in which channel they use. Some send `set-adopt`. Some
never send it and deliver the key only through `mgmt_cfg`, where it matches the
device document's `x_authkey`. **Support both.** In practice `mgmt_cfg` is the
common path.

### Three things that trip up an implementer

1. **Inform continuously, through the whole handshake.** There is no separate
   push channel — the controller can only reply to the device's own next
   inform. A device that stops informing after one attempt never completes
   adoption. Treat any single non-connected reply as "not there yet," never as
   failure.
2. **Gate the `mgmt_cfg.authkey` channel on still holding the default key.**
   Accept a `mgmt_cfg` authkey only while the device's key is still
   `DefaultKey`. Once it holds a real key, ignore any further `mgmt_cfg` authkey
   lines. Applying `mgmt_cfg.authkey` unconditionally is the classic
   stuck-adopt bug: a stray or replayed `mgmt_cfg` clobbers the adopted key back
   to one the controller no longer recognizes, and the device falls to pending.
   `set-adopt`'s key has no such gate — it always applies.
3. **The reported `inform_url` must have an IP-literal host, not a hostname.**
   The controller validates the device-reported `inform_url` after adoption and
   rejects a hostname with `invalid inform_ip <host>` (HTTP 400). A device that
   knows its controller only by DNS name must resolve it to an IPv4 address
   before reporting `inform_url`.

Two related resets arrive as `cmd` replies, handled like `set-adopt`.
**`setdefault`** (factory reset) clears `adopted`, resets the key to
`DefaultKey` and `cfgversion` to `"0"`, drops back to CBC, and forgets any
provisioned config. **`reboot`** and **`upgrade`** both reset the uptime clock
as a real reboot would. `upgrade` also carries a target firmware version the
device adopts. Each needs the device to keep informing afterward for the new
state to reach the controller.

## Capability bitmaps

A controller gates many features on integer bitmaps the device self-reports.
Asking for a feature the device never claimed returns a 404 — for example, BGP
config against a gateway that never reported the routing bit answers
`api.err.BgpUnsupportedDevice`. Four bitmap fields appear on the wire:

- **`fw_caps`** — a top-level int on every inform. The controller tests 22
  distinct bits of it, so it matters. A device with no real bitmap can send a
  deliberate placeholder rather than an arbitrary value:
  `inform.PlaceholderFWCaps` (`3`, bits 0 and 1) is safe because neither bit is
  among the 22 the controller checks, so it reads as a claim to nothing. Don't
  assume any other small value is equally safe.
- **`udapi_caps`** — a top-level int, sent only alongside `udapi_version` (see
  the pairing rule above). Gates the newer UDAPI config-plane features.
- **`switch_caps`** — a nested object, not a single int: several sub-bitmaps
  under `switch_caps.*` (feature caps, STP caps, storm-control caps, IGMP-snoop
  caps, PTP caps), each its own int.
- **`hw_caps`** — a top-level int for physical hardware features (a screen, an
  LCM, a PoE class, an accelerometer) rather than software features.

The repository ships `capability_bits.json`, a dictionary mapping each bitmap's
named bits to their integer values. Which model claims which bit is a separate
per-model fact the controller can't check — it trusts whatever the device
reports — and is out of scope here.

## L2 discovery packet

A device broadcasts a discovery packet on its local Ethernet segment so a
controller on the same segment can find it without knowing its IP. The packet
carries identity — MAC, model, firmware — and nothing needed to adopt. A device
that will be pointed at an inform URL does not need discovery at all.

### Byte layout

```
header (4 bytes):  version(1) | command(1) | payloadLength(2, big-endian)
body:              repeated TLV, each  type(1) | length(2, big-endian) | value
```

`payloadLength` counts the TLV body only, not the 4-byte header. Iterate TLVs
until it is consumed. A decoder that hits an unrecognized type skips `length`
bytes and continues, so an encoder may add fields a decoder doesn't know yet,
and a decoder must not treat an unknown type as an error. Versions 0, 1, and 2
are in use. A v2 packet must carry MAC (type 1), a sequence number of at least 1
(type 18), and a source MAC (type 19).

### TLV types

| Type | Field | Value |
|--:|--|--|
| 1 | MAC | 6 bytes |
| 2 | MAC + IP address | 6 + 4 bytes, repeatable |
| 3 | firmware version | string |
| 10 | uptime | uint32 seconds |
| 11 | hostname | string |
| 12 | platform / model code | string |
| 13 | ESSID | string |
| 14 | wireless mode | uint32 |
| 18 | sequence number (v2) | uint32 |
| 19 | source MAC (v2) | 6 bytes |
| 21 | model (v2) | string |
| 53 | netmask | 4 bytes |

Types 6, 7, and 8 (username, salt, challenge) belong to a controller-initiated
challenge exchange, not a basic identity broadcast. The set above is the
identity core, and a device needs only these to be discovered. The port is UDP
10001.

### Reference encoder

`discovery.Announcement` (`discovery/packet.go`) implements the identity set:
MAC, repeatable MAC+IP addresses, firmware, uptime, hostname, platform, ESSID,
wireless mode, netmask, and, for v2, sequence number, source MAC, and model.
`Marshal` omits any zero-valued field rather than sending an empty TLV. `Parse`
skips any type it doesn't recognize, matching the controller.
