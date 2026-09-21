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

- **Gateway** (`ugw`/`uxg`/`udm`/`ucg`): `if_table`, `config_network_wan`,
  `uplink` (**a string**, not an object), `system-stats` (`cpu`, `mem`,
  `uptime`) and `cfgversion`. Everything else about the gateway's network state
  is derived from `if_table`. Full detail under *Gateway payload* below.
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
- **Power** (PDU, smart plug, power strip, RPS): `outlet_table`,
  `outlet_overrides`, `psu_table`, and a set of device-level power fields. The
  power family is not a device type: these models report `usw`, `uap` or `usp`
  and run the corresponding inform path — see *Power-device payload* below.
- **UPS / battery**: everything a power device sends, plus `vbms_table`. The
  presence of `vbms_table` is what makes a device a UPS on the wire. See *UPS
  and battery payload* below.

### Value coercion

The controller reads each key with a typed accessor, and most of those
accessors coerce. A key it cannot convert falls back to the accessor's default
rather than failing the inform.

| Accessor | Also accepts | On an unconvertible value |
|---|---|---|
| int | a string that parses as a **decimal integer** | the default, usually `0` |
| double | a string that parses as a double | the default |
| boolean | the strings `"true"` / `"false"` | the default |
| strict boolean | nothing but a JSON boolean | the default |
| string | nothing but a JSON string | the default |

Two consequences bite in practice. A `system-stats` block sent as
`{"cpu": "1.5"}` is read by the gateway path with an int accessor — `"1.5"` is
not a decimal integer, so it stores `0`, while the time-series path reads the
same key as a double and stores `1.5`. And a misspelled key is not an error: it
is silently dropped, never reaches the device document, and produces no log
line. Spelling is the whole contract.

### Config the controller pushes back

There are two provisioning shapes on the wire, and a device should implement
both.

**`setstate`.** A reply carrying `radio_table`, `vap_table`, `port_table`, or
`port_overrides` as JSON. A device stashes these and echoes them back verbatim
on every later inform, overriding whatever it would otherwise compute. A device
that takes provisioned config and then reports its own defaults looks, from the
controller's side, exactly like one that rejected the push.

**`setparam`.** A reply carrying `cfgversion`, `cfgtimeout`, `system_cfg`,
`blocked_sta` and `mgmt_cfg`. `system_cfg` and `blocked_sta` are flat,
newline-separated `key=value` text, not JSON — the same encoding `mgmt_cfg`
uses. Current controller builds provision **everything** this way and emit no
`setstate` at all: the tables that used to be pushed as JSON are instead read
back out of the device's own inform and stored (`port_overrides`,
`outlet_overrides`, `config_port_table`, `config_network_wan`,
`config_network_lan`). Do not block waiting for a `setstate`, and ignore an
unknown `_type` gracefully rather than treating it as an error.

Per-family `system_cfg` keys, beyond the switch's port keys and the gateway's
firewall/NAT/DHCP/routing/VPN/QoS paths:

| Key | Value | Family | Meaning |
|---|---|---|---|
| `outlet.<index>.relay_state` | `enabled` / `disabled` | power | Desired state of outlet `<index>`. One line per outlet. |
| `outlet.status` | `enabled` / `disabled` | power | Master outlet enable; mirrors the device's `outlet_enabled`. |
| `beep.status` | `enabled` / `disabled` | power, UPS | Audible alarm; mirrors `beep_enabled`. Gated on `smart_power_caps` bit 4. |
| `power_cycle_on_ac_recovery.status` | `enabled` / `disabled` | power, UPS | Re-power outlets after AC returns. Gated on `smart_power_caps` bit 2. |
| `power_cycle_on_ac_recovery.time` | int seconds | power, UPS | Delay before that re-power. |
| `epo.status` | `enabled` / `disabled` | UPS | Emergency power off; mirrors `vbms_table.epo_enabled`. Gated on `smart_power_caps` bit 16. |
| `ac_input_thd.level` | int | UPS | AC input total-harmonic-distortion threshold; mirrors `vbms_table.input_thd_level`. Gated on `smart_power_caps` bit 64. |
| `nut_server` block | — | UPS | Network UPS Tools server config. Gated on `smart_power_caps` bit 1. |

Booleans in `system_cfg` are rendered as the literal words `enabled` and
`disabled` for these keys. A second renderer emits `true`/`false` elsewhere, so
parse both.

**Turning an outlet on or off is not a command.** It is a configuration change:
the controller edits its stored `outlet_overrides`, bumps `cfgversion`, and
pushes a new `system_cfg` containing `outlet.<n>.relay_state`. The
controller-side REST surface that drives it is a `PUT` of the whole
`outlet_overrides` array to `/rest/device/{_id}`, which three independent public
clients implement identically —
[aiounifi `DeviceSetOutletRelayRequest`](https://github.com/Kane610/aiounifi/blob/7154e750dd0f1c7415b9c390fb2053f251c13a92/aiounifi/models/device.py#L781-L812),
[Home Assistant's outlet switch](https://github.com/home-assistant/core/blob/3c49717915cb1eceabbd95af3782413817ef8d48/homeassistant/components/unifi/switch.py#L198-L204),
and the
[PHP client's worked example](https://github.com/Art-of-WiFi/UniFi-API-client/blob/ce7e6c84a05813ca52a03d5c6e6bac5e039d5b8f/examples/modify_smartpower_pdu_outlet.php)
(read the array, mutate the matching `index`, write the whole array back).
Nothing outlet-shaped exists under `cmd/devmgr`.

**`cfgversion` convergence is the hard requirement.** The device must apply
`system_cfg` and report the received `cfgversion` on its next inform. Until the
reported value matches what the controller sent, the controller re-provisions on
every inform and the device never reaches the connected state. A device that
failed to apply the config says so with a top-level `commit_errors` key; the
controller tests only for its presence.

## Power-device payload

A power device is a rack PDU, a smart plug, a power strip, or a redundant
power supply. Everything here is additive to the adopted payload above.

### There is no "power" device type

The controller resolves a device's family from the `model` string alone,
against an internal model table, before any per-family parsing runs. No field on
the wire names the family, and a `model` the controller does not know is refused
outright. The shipping power products are spread across four unrelated device
types, and none of the classic ones is `usp`:

| Product class | Model codes | `type` on the wire | Inform path that runs |
|---|---|---|---|
| Rack PDU | `USPPDUP`, `USPPDUHD` | `usw` | switch |
| Redundant power supply | `USPRPS`, `USPRPSP` | `usw` | switch |
| Smart plug / strip | `UP1`, `UP6` | `uap` | access point |
| UPS-class | `USPDA2B`, `USPDA2C`, `USPDA29`, `USPDA31` | `usp` | power |
| Battery module | `UPB` | `uble` | Bluetooth peripheral |

The mapping is public. A community redistribution of the device-model catalogue
keys it by `model` string with the same types and capability sets
([`device-models.json`](https://github.com/tnware/unifi-controller-api/blob/bb295b6c162a54b2dbfde3b575c1e05d46103218/unifi_controller_api/device-models.json)),
the [community API reference](https://ubntwiki.com/products/software/unifi-controller/api)
agrees on the four models it lists, and captured payloads confirm it from the
device side: a USP-PDU-Pro reports `type: "usw"`
([`examples/pdu.json`](https://github.com/unpoller/unifi/blob/6f40ea1881efda07c91edd52eedc854594c76697/examples/pdu.json)),
while `UP1`/`UP6` report `type: "uap"` and carry `radio_table`,
`radio_table_stats`, `vap_table` and `wifi_caps` like any access point. The Go
client carries a
[dedicated heuristic](https://github.com/unpoller/unifi/blob/6f40ea1881efda07c91edd52eedc854594c76697/devices.go#L343-L366)
for this, commented "UniFi API sometimes returns type=usw for PDU/UPS devices".

Three consequences for a firmware implementer:

1. Pick a real model code for the product. You cannot select power behaviour
   with a type field, because there is no type field.
2. Send the table set *that type's* handler reads. A PDU on a PDU model code
   should send what a switch sends — `port_table`, `port_overrides`,
   `ethernet_table`, `psu_table`, `fan_table` — **plus** `outlet_table`.
3. A plug on `UP1`/`UP6` runs the access-point path and will be asked for radio
   tables it does not have. Send empty arrays rather than omitting them.

`outlet_table` and `outlet_overrides` are the exception that makes this
workable: they are processed by the stage that runs for **every** device type,
so outlets work whichever type the model resolves to.

### `outlet_table`

Top-level key, array of objects, one per outlet. Copied verbatim into the
controller's device document, so any extra key survives into its REST output
even when nothing parses it. Entries are then joined by `index`.

| Key | Type | Required | Notes |
|---|---|---|---|
| `index` | int | **yes** | 1-based outlet number, and the join key for the override, the metering sample and the config line. An entry without it is skipped entirely. |
| `name` | string | no | When absent the controller substitutes a per-model default (see below). Send it only if the device has a meaningful label. |
| `relay_state` | bool | **yes** on any switchable outlet | `true` = energised. This is what the UI shows as on/off, and it is compared field-by-field against the stored override to decide whether the outlet changed. |
| `cycle_enabled` | bool | no | Outlet participates in power-cycle-on-internet-loss. Genuinely absent from most outlets in real captures — do not assume it is on every row. |
| `outlet_caps` | int bitmap | no | **Per-row**, not device-level. Gates switchable and metered. See *Capability bitmaps*. |
| `outlet_type` | int | no | Legacy outlet class, default `0`. Read only when `outlet_caps < 65536`; `outlet_type == 0` then means AC. |
| `has_relay` | bool | no | Outlet is switchable. Surfaced, not bit-tested — `outlet_caps` is the gate. |
| `has_metering` | bool | no | Outlet reports volts/amps/watts. Same treatment. |
| `button_state` | bool | no | Physical button position. Only meaningful on rows that set `outlet_caps` bit 8; used to reconcile a locally-toggled outlet with the desired state. |
| `outlet_voltage` | number | metered outlets | Volts. |
| `outlet_current` | number | metered outlets | Amps. |
| `outlet_power` | number | metered outlets | Watts. |
| `outlet_power_factor` | number | no | 0–1. Stored and republished; no controller behaviour depends on it. |
| `relay_group` | int | no | Groups outlets sharing one relay. **Unconfirmed** — seen in exactly one public payload, where its value always equals `index`, and modelled by no client. Safe to send, do not rely on it. |

`index`, `name`, `relay_state`, `cycle_enabled`, `outlet_caps`, `has_relay`,
`has_metering` and the four metering keys are declared by
[aiounifi's outlet `TypedDict`](https://github.com/Kane610/aiounifi/blob/7154e750dd0f1c7415b9c390fb2053f251c13a92/aiounifi/models/device.py#L119-L131)
and by the
[Go client's `OutletTable` struct](https://github.com/unpoller/unifi/blob/6f40ea1881efda07c91edd52eedc854594c76697/pdu.go#L122-L134),
and appear in captured USP-PDU-Pro payloads. `outlet_type`, `button_state` and
the `outlet_caps` bits above 2 carry no public source: no client models them and
no fixture shows them. They are stated here without one.

Three facts from real captures that a firmware implementer gets wrong by
guessing:

1. **Metering values are JSON strings on some firmware and numbers on others.**
   A USP-PDU-Pro sends `{"outlet_voltage": "118.566", "outlet_current": "0.061",
   "outlet_power": "3.815", "outlet_power_factor": "0.527"}`; a UPS sends the
   same keys as numbers. Both client libraries are explicitly built to accept
   either, and the controller's double accessor parses a numeric string. **Send
   numbers** — they work on every consumer — but a device-side parser must
   tolerate strings.
2. **Metering keys are omitted, not zeroed, on a non-metering outlet.** In the
   captured USP-PDU-Pro payload the four USB outlets (`outlet_caps: 1`) carry
   none of the four keys; the sixteen AC outlets (`outlet_caps: 3`) carry all
   four.
3. **`outlet_caps` and `has_relay`/`has_metering` are disjoint in every real
   payload.** `UP1`/`UP6` send the booleans and no `outlet_caps`; USP-PDU-Pro
   and the UPS models send `outlet_caps` and no booleans. Consumers prefer the
   booleans when present and fall back to the bitmap
   ([aiounifi `outlet.py`](https://github.com/Kane610/aiounifi/blob/7154e750dd0f1c7415b9c390fb2053f251c13a92/aiounifi/models/outlet.py#L32-L66)),
   so sending both is harmless. Sending neither leaves both unknown, not false.

Default outlet names, when `name` is omitted:

| Model | Rule |
|---|---|
| `USPPDUP` | `index ≤ 4` → `USB Outlet <n>`, else `Outlet <n>` |
| `USPPDUHD` | `13 ≤ index ≤ 16` → `USB Outlet <n>`, else `Outlet <n>` |
| `UP6` | `index > 6` → `USB Outlets` (literal, unnumbered), else `Outlet <n>` |
| anything else | `Outlet <n>` |

Minimal entry, and a metered entry:

```json
{"index": 1, "relay_state": true, "outlet_caps": 3}

{"index": 5, "name": "Outlet 5", "relay_state": true, "cycle_enabled": true,
 "outlet_caps": 65539, "outlet_voltage": 119.8, "outlet_current": 0.42,
 "outlet_power": 50.3, "outlet_power_factor": 0.99}
```

### `outlet_overrides`

Top-level key, array of objects. This is the *configuration* side of the outlet
table — the controller's record of what each outlet should be, and the thing an
operator edits. Exactly four keys, all optional:

| Key | Type | Notes |
|---|---|---|
| `index` | int | Joins to `outlet_table[].index`. |
| `name` | string | Operator-chosen label. |
| `relay_state` | bool | Desired on/off. |
| `cycle_enabled` | bool | Include in power-cycle-on-internet-loss. |

Both directions matter. On adoption the controller seeds its own override list
from the reported `outlet_table` (taking `index` and `relay_state`) when it has
none, so a device that reports overrides on its first adopted inform hands the
controller its current state as the starting configuration. Afterwards, a
reported `outlet_overrides` is how the controller learns about a *local* change
— a front-panel button or an on-device display. The same reporting path also
accepts a reported `beep_enabled` and a reported battery-management override.

**Entries are heterogeneous and the array is not 1:1 with `outlet_table`.** In a
captured 20-outlet USP-PDU-Pro payload, 15 entries carry only
`{index, relay_state}` and 5 carry all four keys; `name` appears only where the
outlet was renamed and `cycle_enabled` only where it was ever configured. A
`UP1` capture has an *empty* overrides array beside a one-entry `outlet_table`.
Firmware must not assume every entry carries every key, nor that lengths match
([aiounifi's `TypedDict` is `total=False`](https://github.com/Kane610/aiounifi/blob/7154e750dd0f1c7415b9c390fb2053f251c13a92/aiounifi/models/device.py#L107-L115);
[Go `OutletOverride`](https://github.com/unpoller/unifi/blob/6f40ea1881efda07c91edd52eedc854594c76697/pdu.go#L116-L121)).

The controller never pushes `outlet_overrides` back as JSON. It pushes the
desired relay state inside `system_cfg` — see *Config the controller pushes
back*.

### `psu_table`

Top-level key, array of objects, one per power supply unit, read by both the
common and the switch inform paths.

| Key | Type | Notes |
|---|---|---|
| `index` | int | PSU slot number. |
| `label` | string | PSU label. |
| `online` | bool | Present and running; also drives PSU health. |
| `psu_type` | int bitmap | PSU class. Shares its vocabulary with the device-level `power_source` — see *Capability bitmaps*. |
| `psu_caps` | int bitmap | PSU capability bits. |
| `voltage` | double | Volts. |
| `current` | double | Amps. |
| `power` | double | Watts. |
| `power_nominal_design` | double | Rated watts. |
| `temperature` | double | Degrees C. |
| `charge_status` | string | Battery PSU only. |
| `energy_full_design` | double | Battery PSU only. |
| `capacity` | int (%) | Battery PSU only. |
| `health` | string | Battery PSU only. |
| `time_to_empty_avg` | long (seconds) | Battery PSU only; averaged runtime remaining on battery. |

The last five are the battery-PSU extension — omit them on a mains-only supply.
A `psu_table` row carrying them is understood as a battery supply even on a
device with no `vbms_table`.

**No public source carries `psu_table`** — it appears in no fixture and is
modelled by no third-party client, which also means no third-party consumer
will read it. The shape below is stated without a source.

### Device-level power fields

All top-level. The controller bulk-copies `power_source`,
`power_source_voltage`, `psu_table`, `power-monitor`, `total_max_power`,
`led_state` and `outlet_table` into its device document on every inform, for
every device type. **Note the hyphen in `power-monitor`** — it is not
`power_monitor`.

| Key | Type | Notes |
|---|---|---|
| `power_source` | int bitmap | How the device is powered. Bit-tested by the power-budget logic and matched against `power_budget_table` rows. If the real value is unknown, send `0`; the controller treats unknown as undefined and degrades quietly. |
| `total_max_power` | int (watts) | Total power the device can supply. Feeds the power budget and is republished as `total_max_power` / `total_max_effective_power` / `total_used_power`. |
| `system_max_power_usage` | int (watts) | Peak system draw; written through on change. |
| `poe_power_used` | double (watts) | PoE power currently delivered. Switch/AP side, not PDU. |
| `smart_power_caps` | int bitmap | Gates which power/UPS keys the controller generates at all. See *Capability bitmaps*. |
| `outlet_enabled` | bool | Master outlet enable. The controller owns this one — the device need not originate it, but it must survive round-tripping. Defaults to `true`. |
| `beep_enabled` | bool | Audible alarm. Accepted from the device on a local-change report and pushed back as `beep.status`. |
| `general_temperature` | int | Device temperature; read only when the model claims a temperature sensor. |
| `overheating` | bool | Thermal alarm. |
| `temperatures` | array | Sensor readings; republished untouched. Public captures show `{name, type, value}` entries. |
| `fan_table` | array | Per-fan: `index` (int), `present` (bool), `critical_state` (int bitmap — high-speed, stopped, unavailable, wrong-direction), `status`. |
| `usbpd` | object | USB Power Delivery: `mode` and `active_port`, both enum strings. Unresolvable values are logged and dropped, not fatal. |
| `power_budget_table` | array | Per-source budgets: `power_source` (int, matched against the device's own), `power_budget` (int watts), `avail_budget` (int watts), `max_cable_loss` (int watts). |
| `power_source_voltage` | number | Input voltage of the active source. **Unconfirmed** — stored but no controller behaviour reads it. |
| `led_state`, `power-monitor` | opaque | Stored and republished only. **Unconfirmed** as inputs to anything. |

Three more device-level keys appear in every captured power payload and are read
by third-party consumers, though no controller behaviour depends on them:
`outlet_ac_power_budget` and `outlet_ac_power_consumption` — **decimal strings**
on the wire, e.g. `"1875.000"` and `"307.741"` — and
`outlet_power_cycle_on_ac_recovery_enabled` (bool)
([Go `PDU` struct](https://github.com/unpoller/unifi/blob/6f40ea1881efda07c91edd52eedc854594c76697/pdu.go#L60-L76);
[aiounifi](https://github.com/Kane610/aiounifi/blob/7154e750dd0f1c7415b9c390fb2053f251c13a92/aiounifi/models/device.py#L540-L544);
Home Assistant exposes the first two as the device's power sensors). Send them
as strings if you want those consumers to work.

### `rps`

Top-level object, read by the **switch** inform path — which is the path an RPS
unit runs, since the RPS models are typed `usw`. A switch wired to an RPS
reports its own redundant supply the same way.

Copied out of the object: `power_supply_12v`, `power_supply_54v`,
`power_remaining_12v`, `power_remaining_54v`, `power_delivering_12v`,
`power_delivering_54v`, `power_management_mode`, `anomalies` (long; bit-tested
when deciding whether to raise an event), `anomalies_details`,
`oring_poe_warning_flag`, `oring_poe_warning_level`,
`oring_poe_warning_percentlabel`, plus `rps_port_table`.

Each `rps.rps_port_table[]` entry: `port_idx` (int, the join key and the target
of `rps-port-recovery`), `name`, `peer` (object containing `model`), `up`,
`anomalies` (int), `high_priority`, `port_mode` (string), `port_state`,
`port_error_disabled` (bool), `power_active`, `power_delivering` (bool),
`power_required_12v` / `power_required_54v` (int), and per rail
`power_12v_voltage` / `power_12v_current` / `power_12v_power` and the `54v`
equivalents (doubles), plus `power_12v_batt_guard` / `power_54v_batt_guard`.

No public source shows an RPS device payload, so this shape is stated without
one. Note also that the plain `USP-RPS` has no outlets at
all — only the Pro variant adds a single switchable outlet.

## UPS and battery payload

### What makes a device a UPS

Nothing on the wire says so directly. Two things do it together:

1. **The `model` string.** The controller's model table carries the flags that
   matter — battery present, battery management, pure sine wave, smart outlet,
   outlet metering, paired-device support. A UPS model carries the battery
   flags; a PDU model carries the outlet flags. The controller never infers
   either from the payload.
2. **The presence of `vbms_table`.** A PDU sends `outlet_table`; a UPS sends
   `vbms_table`. The entire battery pipeline is driven off that key being
   present — absent, nothing battery-related is stored or evaluated for that
   inform. A UPS that also has switchable outlets sends both, and both paths
   run.

The same discriminator is what every public consumer uses: the Go exporter gates
all battery metrics on `vbms_table` being non-nil, and Home Assistant removes
its UPS entities outright when `battpool` is absent. Do not look for an `is_ups`
flag or a UPS-only `type` value — the UPS models are split across `usp` and
`usw` just as the PDUs are.

A third, independent signal: a `psu_table` row carrying `charge_status`,
`capacity`, `health`, `energy_full_design` and `time_to_empty_avg` is understood
as a battery supply even with no `vbms_table`.

### `vbms_table`

Top-level key, a JSON **object** despite the name, read as an optional
sub-document. Two consumers read it on every inform: one builds the published
battery model, one evaluates alarms.

| Key | Type | Required | Notes |
|---|---|---|---|
| `battpool` | object | **yes** for a useful UPS | Aggregate pool state. When missing, the pool is built with all zeros. |
| `is_battery_mode` | bool | **yes** | The device is running on battery. Default `false`. This is the AC-lost signal; it drives the "battery power in use" and "AC power restored" alerts. |
| `battery_table` | array | no | Per-module detail. |
| `bms_run_anomaly` | int bitmap | no | Battery-management anomaly bits; bit-tested. |
| `epo_enabled` | bool | no | Emergency power-off armed. Also accepted back from the device as a local override and re-emitted as `epo.status`. |
| `input_thd_level` | int | no | AC input total-harmonic-distortion level; re-emitted as `ac_input_thd.level`. |

Declared by
[aiounifi](https://github.com/Kane610/aiounifi/blob/7154e750dd0f1c7415b9c390fb2053f251c13a92/aiounifi/models/device.py#L154-L161)
and, for the first four keys, by the
[Go `VBMSTable` struct](https://github.com/unpoller/unifi/blob/6f40ea1881efda07c91edd52eedc854594c76697/pdu.go#L137-L142).
`epo_enabled` and `input_thd_level` have a single public witness.

### `vbms_table.battpool`

This is the object that answers charge, runtime, load and capacity. Note the
mixed naming — `batteryLevel` and `timeToRemain` are camelCase,
`batt_available_cnt` is snake_case, `ischarging` and `capWh` are neither.
Reproduce them exactly.

| Key | Type | Notes |
|---|---|---|
| `batteryLevel` | int (%) | State of charge, 0–100. |
| `batteryLevelLow` | int (%) | Low-battery threshold. |
| `batteryLevelLowest` | int (%) | Critical / shutdown threshold. |
| `timeToRemain` | int (seconds) | Estimated runtime remaining. |
| `ischarging` | bool | Pack is charging. |
| `capWh` | int (Wh) | Pack energy capacity. |
| `batt_available_cnt` | int | Batteries currently available. |
| `batt_available_power` | int (W) | Power available from the pack. |
| `batt_total_power` | int (W) | Total pack power. |
| `readycnt` | int | Batteries in the ready state. |
| `device_total_power_budget` | int (W) | Device-wide power budget. |
| `device_total_power_output` | number (W) | Current load, in watts. |

Four more keys appear in public payloads and are read by third-party consumers
but drive no controller behaviour: `device_total_power_factor` (0–1),
`device_output_voltage`, `device_output_current`, and — mutually exclusive by
model — `device_input_voltage` (mains in) or `device_bypass_voltage`
([aiounifi `TypedDeviceVBMSBattPool`](https://github.com/Kane610/aiounifi/blob/7154e750dd0f1c7415b9c390fb2053f251c13a92/aiounifi/models/device.py#L136-L150);
[Go `BattPool`](https://github.com/unpoller/unifi/blob/6f40ea1881efda07c91edd52eedc854594c76697/pdu.go#L145-L157)).
A fifth, `battery_avr_time`, is declared by both clients, observed as `-1`, and
explained by neither.

The controller republishes these under different names in its own REST output
(`batteryCurrentLevel`, `timeToRemaining`, `deviceTotalPowerOutput`, …). Those
spellings belong to what the controller publishes, not to what a device sends:
a device using them reports nothing the controller will read.

**Load percentage is derived, not reported.** The public exporter computes it as
`device_total_power_output / device_total_power_budget × 100`
([unpoller](https://github.com/unpoller/unpoller/blob/d46146bdd08164d6e42792b091f2e92b795e2a7d/pkg/promunifi/pdu.go#L252-L257)).
There is no load-percent key on the wire.

### `vbms_table.battery_table[]`

One entry per battery module: `id` (string), `model` (string, used to filter
which modules are real), `fwv` (string), `batteryHealth` (int),
`batteryAnomaly` (int bitmap), `batteryMV` (int millivolts), `batteryMA` (int
milliamps), `health` (string enum `Good` | `Bad` | `Unknown`, matched
case-sensitively), `isBadBattery` (bool), `uptime` (int seconds), `isLocating`
(bool), `client_state` (lowercase enum: `init`, `connect`, `disconnect`,
`found`, `bind`, `ready`, `update`, `unknown`), `client_update_progress`
(int %), `client_update_result` (lowercase enum: `init`, `running`, `success`,
`failed`, `timeout`, `unknown`), `upgradable` (bool), `upgrade_to_firmware`
(string), `upgradeDuration` (int seconds).

An unknown enum string resolves to the `unknown` member rather than failing the
parse.

The *key* `battery_table` is public; its **element shape is not**. Both public
clients type it as an opaque list and it is empty in every public fixture, so
the field list above carries no public source and is stated without one.

### `bms_run_anomaly` bits

| Value | Meaning |
|---:|---|
| `64` | Overloaded, exceeding 120 % |
| `1024` | Overloaded, between 100 % and 120 % |
| `4096` | Battery low |
| `16384` | Discharge time limit reached |

Each of these raises an operator-visible alert. Set only bits the device can
actually justify.

### Where the electrical measurements live

There is no `input_voltage` / `output_voltage` pair, and no NUT-style status
table. The measurements are distributed:

- **Load, watts** — `vbms_table.battpool.device_total_power_output`.
- **Per-module voltage and current** — `vbms_table.battery_table[].batteryMV`
  and `.batteryMA`. These two are scaled: millivolts and milliamps, unlike
  their unscaled neighbours in `battpool`.
- **Output volts and amps** — `vbms_table.battpool.device_output_voltage` /
  `.device_output_current`.
- **PSU-level volts, amps, watts** — `psu_table[].voltage` / `.current` /
  `.power`.
- **Outlet-level volts, amps, watts** — `outlet_table[].outlet_voltage` and
  friends, on a UPS with metered outlets.
- **AC input quality** — `vbms_table.input_thd_level`.

Do not send `battery_charge`, `battery_runtime`, `on_battery`,
`load_percentage`, `input_voltage`, `output_voltage`, or any `ups_*` status
table. Nothing reads them. There is likewise no evidence of any consumer parsing
an input-frequency, output-frequency, nominal-voltage or transfer-voltage field.

### `nut_client_table`

Top-level array, copied verbatim into the device document by both the power and
the switch paths. It is the list of NUT clients the UPS is serving — the hosts
registered with its NUT server for graceful shutdown. The *key* is read; **no
field inside an entry is parsed by anything**. Send whatever the real device
sends.

The companion `nut_server` is the controller-pushed side and is never read out
of an inform.

### Graceful shutdown

The only wire inputs are `vbms_table.is_battery_mode` and
`vbms_table.battpool.timeToRemain`. When the device reports battery mode and
runtime below the configured threshold, the controller shuts down the paired
consoles through its own channels. Nothing is sent to the UPS, and the
device does not report the shutdown threshold or its timestamps.

## Gateway payload

### `uplink` is a string

This is the trap, and it is settled: **a gateway reports `uplink` as a string,
the `name` of an `if_table` entry.** Sending an object does nothing.

The mechanism, which explains why the question comes up at all:

1. The controller reads `uplink` out of the inform with a **string** accessor.
   There is no object-valued read of `uplink` from an inform anywhere.
2. The default it passes is the interface name of the configured WAN network
   group, so a gateway that omits `uplink` entirely still gets its WAN
   interface selected — from the controller's own configuration.
3. That name is matched against `if_table[].name`. The same lookup also accepts
   a link-aggregation entry (`aggregate_status`, `lag_dev`).
4. The matched `if_table` entry becomes the **object** the controller stores and
   republishes as `uplink`, after adding `max_speed`, `type: "wire"` and, on the
   generic path, `speed`, `full_duplex` and `port_idx`.

So the rich `uplink` object visible in the controller's REST output — the one
with `gateways`, `nameservers`, `latency`, `xput_down`, `speedtest_*`
([Go `Uplink`](https://github.com/unpoller/unifi/blob/6f40ea1881efda07c91edd52eedc854594c76697/usg.go#L75-L112),
confirmed by two captured gateway payloads) — is **constructed by the
controller**, never taken from the wire. `"uplink": "eth0"` is correct and
sufficient.

The same string-then-lookup shape applies to other device types: the common path
falls back to `if_table` and then to the device's port stats, and the power
path defaults the string to `"eth0"`. A downstream device's uplink object has a
different shape again (`uplink_mac`, `uplink_remote_port`, `media`, `max_vlan`),
which is why the two public clients model `uplink` differently — they model
different device classes, and both are right.

### What a gateway must send

- `if_table` — the authoritative interface list. Everything else about the
  gateway's network state is derived from it.
- `config_network_wan` — without it the controller logs the missing key and
  **skips WAN processing entirely** for that inform, so the WAN never
  bootstraps.
- `uplink` — the string above.
- `system-stats` — `cpu`, `mem`, `uptime`.
- `cfgversion` — echoed from the last `setparam`.

Should send: `network_table`, `config_port_table`, `port_overrides`,
`config_network_lan`, `config_network_wan2` on dual-WAN models, `usg_caps` and
the `has_*` feature flags.

Need not bother sending:

- `port_table` — **the controller builds the gateway's port table itself.**
- `ethernet_table` — **the controller derives it from `if_table`.**
- `dhcp_server_table` — read only on the *switch* path, where it means rogue
  DHCP servers seen on the wire. The gateway path never reads it.
- `speedtest-status` — only after the controller has asked for a speed test.

### `if_table`

Top-level array of objects. At least eight distinct controller stages read it.

| Key | Type | Required | Notes |
|---|---|---|---|
| `name` | string | **yes** | Interface name (`eth0`, `pppoe0`, `br0`). The join key for `uplink`, `network_table[].ifname` and `config_port_table[].ifname`. |
| `ip` | string | **yes** | IPv4 address; also compared to detect WAN IP changes. |
| `netmask` | string | yes | IPv4 netmask. |
| `mac` | string | yes | Interface MAC; the join key when building `ethernet_table`. |
| `up` | bool | yes | Link up. |
| `speed` | int (Mbit/s) | yes | Negotiated speed. |
| `full_duplex` | bool | yes | Duplex. |
| `enable` | bool | no | Administratively enabled; default `true`. |
| `num_port` | int | no | Physical ports behind this interface. Drives `ethernet_table`. |
| `nameservers` | array of string | WAN interfaces | Become the uplink's `dns`. |
| `gateways` | array of string | WAN interfaces | Element 0 becomes the uplink's `gateway`. |
| `latency` | double (ms) | no | Feeds the gateway latency time series for the uplink interface. |
| `time_delta` | double (seconds) | no | Seconds since the previous sample; lets the controller compute rates. |
| `rx_bytes`, `tx_bytes`, `rx_packets`, `tx_packets` | long | yes | Counters. |
| `rx_dropped`, `tx_dropped`, `rx_errors`, `tx_errors`, `rx_multicast` | long | no | Counters. |
| `rx_bytes-r`, `tx_bytes-r`, `bytes-r` | double (bytes/s) | no | Rates. |
| `uptime` | int (seconds) | no | Interface uptime; read by the uplink-age check. |
| `aggregate_status` | object | no | LAG status; makes the entry match a LAG name in the uplink lookup. |
| `lag_dev` | string | no | LAG member device name; alternative match key. |

If the device sends `time_delta`, `rx_bytes` and `tx_bytes`, the controller
computes the three `-r` rate fields itself. Sending them is legal; sending
`time_delta` is less work.

An interface's entry supplies the port-table entry the controller builds from
it, and it does so by name, so these are the fields that reach the UI and the
only ones worth populating accurately. Link state and addressing: `ip`,
`netmask`, `mac`, `up`, `speed`, `full_duplex`. Counters: `rx_bytes`,
`rx_dropped`, `rx_errors`, `rx_packets`, `tx_bytes`, `tx_dropped`, `tx_errors`,
`tx_packets`, `rx_multicast`. Anything else in the entry stays behind.

### `config_port_table`, and the tables the controller builds

The gateway's `port_table` is assembled by the controller from three inputs: a
per-model default port list, optionally overridden by the device's reported
`config_port_table`; the live `if_table`, joined on
`if_table[].name == portEntry.ifname`; and `network_table`, joined by name,
contributing `nameservers` → `dns` and `gateways` → `gateway` for WAN entries.
The result is stored with `max_speed` from the model and `type: "wire"`. A
gateway that sends its own `port_table` simply has it replaced.

`ethernet_table` is derived the same way: `mac`, `name` and `num_port` from each
`if_table` entry, merged with any previously-stored entry whose `mac` matches
and whose `num_port` is non-zero. To get a sensible one, make sure each physical
`if_table` entry carries `mac` and `num_port`.

`config_port_table` is a top-level array, the device's statement of which
logical port maps to which interface and network group. The controller drops its
stored copy and replaces it with the reported one on every inform — a gateway
that stops reporting it loses its port layout.

| Key | Type | Notes |
|---|---|---|
| `ifname` | string | Joins to `if_table[].name`. |
| `networkgroup` | string | `WAN`, `WAN2`, `LAN`, `LAN2`…`LAN8`, `MGMT`, or `none`. |
| `switch` | string | Internal switch the port hangs off (`switch0`), or `none` for a directly attached port. |
| `name` | string | Port label; this is what the operator sees. |

### `network_table`

Top-level array describing the *networks* configured on the gateway, as distinct
from the physical interfaces.

| Key | Type | Notes |
|---|---|---|
| `name` | string | Network / bridge name, e.g. `br0`. The join key. |
| `ifname` | string | Joins to `if_table[].name`. |
| `ip` | string | IPv4 address on this network. |
| `mac` | string | MAC. |
| `nameservers` | array of string | Become the uplink's `dns`. |
| `gateways` | array of string | Element 0 becomes the uplink's `gateway`. |
| `addresses` | array of string | **IPv6 addresses.** The controller filters to the entry named `br0`, flattens its `addresses`, and stores the result as the device's `ipv6`. This is the only route by which a gateway's IPv6 addresses reach the controller. |

Captured gateway payloads show a much wider entry — the `dhcpd_*` family
(`dhcpd_enabled`, `dhcpd_start`, `dhcpd_stop`, `dhcpd_leasetime`,
`dhcpd_dns_enabled`, `dhcpd_gateway_enabled`, …), `ip_subnet`, `purpose`,
`networkgroup`, `vlan`, `vlan_enabled`, `is_guest`, `is_nat`, `domain_name`,
`num_sta`, and per-network counters
([`examples/ugw.json`](https://github.com/unpoller/unifi/blob/6f40ea1881efda07c91edd52eedc854594c76697/examples/ugw.json);
[aiounifi `TypedDeviceNetworkTable`](https://github.com/Kane610/aiounifi/blob/7154e750dd0f1c7415b9c390fb2053f251c13a92/aiounifi/models/device.py#L70-L104)).
`purpose` (`"wan"`) and `networkgroup` (`WAN` / `WAN2`) are what mark a row as
the WAN. Those keys are stored and republished; `address`, `dhcpv6_pd` and
`stats` are carried but **unconfirmed** as inputs to anything.

### `config_network_wan` and `config_network_wan2`

Top-level objects, and load-bearing: absent, WAN processing is skipped for the
inform and nothing else in the payload compensates. If an operator has changed
the WAN config through the controller API, a flag on the device document makes
the controller ignore the device-reported config instead; otherwise the reported
config is converted into the controller's WAN network configuration, each key
mapped to a `wan_`-prefixed field.

Read as **strings**, skipped when empty: `type`, `ip`, `netmask`, `gateway`,
`username` (PPPoE), `dns1`, `dns2`, `dns3`, `dns4`, `load_balance_type`
(`weighted` | `failover-only`), `type_v6`, `ipv6`, `gateway_v6`,
`dslite_remote_host`.

Read as **ints**, only when present: `vlan`, `smartq_up_rate`,
`smartq_down_rate`, `dhcpv6_pd_size`, `load_balance_weight`, `prefixlen`,
`egress_qos`.

Read as **booleans**, only when present: `vlan_enabled`, `smartq_enabled`.

Read specially: `x_password` (PPPoE password, taken only when non-empty),
`dhcp_options` (array, taken only when non-empty), and `mac_override` (string —
when non-empty the controller sets the override *and* enables MAC-override).

`type` vocabulary: `dhcp`, `static`, `pppoe`, `disabled`, `dslite`,
`dslite-over-pppoe`, and the MAP-E variants whose wire values are
`map-e,hubspoke`, `map-e,jpix` and `map-e,ntt`.

`type_v6` vocabulary: `disabled`, `static`, `dhcpv6`, `slaac`.

DHCP is the trivial case, and `{"type": "dhcp"}` alone is enough to clear the
missing-key warning and bootstrap the WAN. The remaining fields are what the
controller learns *from the device* about a manually configured WAN:

```json
"config_network_wan": {
  "type": "static",
  "ip": "203.0.113.24", "netmask": "255.255.255.0", "gateway": "203.0.113.1",
  "dns1": "203.0.113.1", "dns2": "9.9.9.9",
  "type_v6": "disabled"
}
```

```json
"config_network_wan": {
  "type": "pppoe",
  "username": "user@isp", "x_password": "secret",
  "ip": "198.51.100.77", "netmask": "255.255.255.255",
  "gateway": "198.51.100.1", "dns1": "198.51.100.1",
  "vlan_enabled": true, "vlan": 7,
  "type_v6": "disabled"
}
```

`config_network_wan2` has the same shape, read as an optional sub-document on
dual-WAN models; whether the second WAN port is enabled is governed by a
device-reported `ugw3_wan2_enabled` boolean.

**No public source carries `config_network_wan`**, so what follows is stated
without one — it appears in no client, no fixture and no captured
`/stat/device` payload, which is unsurprising: it is an inform-side key, and the
public corpus is built from the controller's REST output. Captured payloads
carry a *different*, much smaller `config_network` (`{"ip", "type"}`) that is
not the same field. The WAN port-level settings `autoneg`, `fec`, `full_duplex`
and `speed` are in the vocabulary but **unconfirmed**.

### `config_network_lan`

Top-level object. The controller compares it against its own default LAN network
and adjusts (or warns about) the site's LAN configuration when the device
reports something non-default.

| Key | Type | Notes |
|---|---|---|
| `cidr` | string | LAN subnet as `a.b.c.d/nn`. |
| `dhcp_enabled` | string `"true"` / `"false"` | Read with a **string** accessor, not a boolean one. |
| `dhcp_range_start` | string | `ip/prefix` form; the controller splits on `/` and keeps the address. |
| `dhcp_range_stop` | string | Same form. |
| `vlan` | int | LAN VLAN id; when present the controller also enables VLAN on the network. |

### `system-stats`

Top-level object, distinct from the generic `sys_stats` every adopted device
sends. Both go on the wire.

| Key | Type read | Meaning |
|---|---|---|
| `cpu` | read as an integer when the gateway is processed, as a double when sampled for the time series | CPU percentage |
| `mem` | same two readers as `cpu` | Memory percentage |
| `uptime` | int | Seconds since boot |

Send integers — see *Value coercion*. `"1.5"` reads as `0` on the int path.

### `speedtest-status`

Top-level object, reported after a `speed-test` command.

| Key | Type | Notes |
|---|---|---|
| `status_summary` | int | Overall state. A terminal value plus a fresh `rundate` is what makes the result "saved". |
| `status_download` | int | Download phase state. |
| `status_upload` | int | Upload phase state. |
| `status_ping` | int | Latency phase state. |
| `rundate` | long (epoch seconds) | Read as a long here and as an int elsewhere — send a value that fits an int. |
| `source_interface` | string | The interface tested, as `if!<ifname>`. The controller strips `if!` and republishes the remainder as `interface_name`. The prefix is not optional. |
| `latency` | number | Measured latency, ms. |
| `xput_download` | number | Download throughput. |
| `xput_upload` | number | Upload throughput. |

All nine keys except `source_interface` are confirmed by two captured gateway
payloads and modelled by both public clients
([Go](https://github.com/unpoller/unifi/blob/6f40ea1881efda07c91edd52eedc854594c76697/usg.go#L158-L172);
[aiounifi](https://github.com/Kane610/aiounifi/blob/7154e750dd0f1c7415b9c390fb2053f251c13a92/aiounifi/models/device.py#L404-L415)).
Those captures also carry a sibling `speedtest-status-saved` boolean.

The controller-side companions `speedtest-status-1` / `-2` and
`speedtest-pending-interfaces` are never sent by a device.
`speedtest-status-udapi` is a separate, **list**-valued top-level key — send it
only if the device genuinely runs UDAPI.

### Other gateway tables

All are top-level keys the controller reads; the sub-fields are as noted.

- **`num-routes`** (object): `connected`, `static`, `total`, all ints.
- **`routes`** (array): each entry needs both `pfx` (string, destination prefix)
  and `nh` (array). Each `nh` entry contributes `via`, `intf`, `metric` and `t`,
  all strings. Stored as the device's `routing_table`.
- **`users`** (object): `local` (array of `{user, host, tty, uptime (string),
  idle (int)}`). An empty `tty` or `ttyS0` is treated as a console session.
- **`discover`** (object): `devices` (array of `{addresses, fwversion, hwaddr,
  ipv4, product, uptime}`) — the gateway relaying its own L2 discovery results.
- **`ddns-status`** (object): per entry `host_name`, `atime`, `mtime`,
  `warned_min_error_interval` (all longs). The controller derives
  `dynamicdns_table` from it and writes back `updating` and `last_updated`.
- **`pfor-stats`** / **`upnp-stats`** (arrays): drive `portforward_table`. Per
  entry `id` plus `rx_bytes`, `tx_bytes`, `rx_packets`, `tx_packets` (longs).
- **`dpi-stats`** and **`dpi_stats`** (arrays): both spellings are read, by
  different stages.
- **`vpn`** (array): read as a list. Per-entry fields **unconfirmed**.
- **`uptime_stats`** (object): per WAN group (`WAN`, `WAN2`, `WAN_UNBOUND`) —
  `monitors` (array of `{type, target, availability, latency_average}`),
  `alerting_monitors` (same entry shape), `latency_average`, `uptime`,
  `downtime`. The nested `monitors` form is the one confirmed by a captured
  gateway payload; a flat `{availability, latency_average, time_period}` form is
  declared by one public client and appears in no capture, so treat the flat
  form as **unconfirmed**.
- **`wan_magic_stats`** (object): read as an optional sub-document; contents
  **unconfirmed**.
- **`internet`** (bool), **`routing_mac`** (string), **`locating`** (bool): all
  top-level, all read on every inform.

`gateway_mac` and a gateway-level `netmask` are in the controller's vocabulary
but no read of either from an inform was observed — **unconfirmed**.

### How gateway config is pushed and echoed back

A gateway is provisioned through exactly the same envelope as a switch: there is
no gateway-specific push channel. The difference is only in the content of
`system_cfg`, whose keys for a gateway are configuration paths — firewall, NAT,
DHCP server, static routes, VPN, DPI, port forwarding, SNMP, syslog, smart
queue, load balancing — rather than the switch's port keys. Three narrower
`setparam` variants also occur: `preprovision_system_cfg` (config sent before
the device is fully adopted), a `mgmt_cfg`-only update (inform URL change,
authkey rotation, AES-GCM switch), and a `blocked_sta` + `interval` update.

What the gateway must echo:

1. **`cfgversion`**, from the last `setparam`. While it differs from what the
   controller expects, the controller logs both versions and re-provisions on
   every inform, and the gateway never reaches the connected state. This is the
   single hard requirement.
2. **`config_network_wan`**, on every inform, or WAN processing is skipped.
3. **`config_port_table`**, on every inform, or the port layout is lost.
4. **`port_overrides`**, read from the inform with a list accessor and stored as
   the device's port overrides. Echo back what was provisioned.
5. **`inform_url`** with an IP-literal host. A change triggers a
   management-config update and a re-provision.

`ethernet_overrides` (`{ifname, networkgroup}` entries) is controller-side only:
it is read from the controller's own stored device document, not from the
inform, and rewritten when the network-group-to-interface mapping changes. A
gateway does not report it.

**WAN failover is not pushed as a command and is not reported as one.** It is
expressed in the WAN network configuration inside `system_cfg` — failover
priority, load-balance type (`weighted` or `failover-only`), load-balance weight
— and observed back through `uptime_stats` and the gateway's own WAN-transition
events. No `wan-failover` command exists.

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
4. The controller keeps pushing `setparam` replies (`mgmt_cfg`, then
   `system_cfg`) and, on builds that emit them, `setstate` config tables as
   informs continue. The device is connected once a reply arrives to an inform
   that was already sent adopted.

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

## Commands

### Reply envelopes

Every reply to an inform is a single JSON object whose `_type` selects the
envelope:

| `_type` | When | Extra keys |
|---|---|---|
| `noop` | Nothing to do — the overwhelmingly common reply. | `interval` (int, seconds until the next inform) |
| `setparam` | Provisioning or management-config push. | `cfgversion`, `cfgtimeout`, `system_cfg`, `preprovision_system_cfg`, `blocked_sta`, `mgmt_cfg`, `interval`, whichever apply |
| `cmd` | A command is waiting for the device. | `cmd`, plus whatever keys that command takes |
| `reboot` | Restart on this inform. | `soft` (bool), `delay` (long, seconds) |
| `upgrade` | Install firmware. | `url` (string), `version` (string) |
| `setdefault` | Return to factory state. | — |
| `geo_ip_update` | Refresh the GeoIP database. | the database itself |

`server_time_in_utc` (milliseconds since the epoch) is stamped onto **every**
reply whatever the `_type`, and is never absent. A device may use it as a clock
source.

A `cmd` reply has no nesting — the command's own keys sit at the top level
beside `_type` and `cmd`:

```json
{"_type": "cmd", "cmd": "<name>", "...": "...", "server_time_in_utc": 1710000000000}
```

### Power and UPS commands

| Command | Payload | Expected device response |
|---|---|---|
| `relayctl` | `outlet_table` — a selection list whose entries carry an `index` and nothing else; `time` (epoch milliseconds, **as a string**); optionally `period`, `action`, `delay_time_to_off`, `delay_time_to_on` | Act on the outlets named in the selection, then report the resulting `outlet_table[].relay_state`. There is no acknowledgement field — the reported relay states are the acknowledgement. One variant of this command carries no selection list at all, so treat an absent `outlet_table` as valid rather than malformed. |
| `power-cycle` | `port` (int, 1-based PoE port), `unit_id` (int, **omitted entirely** when not stacked) | Drop and restore PoE on that port; reflect the transient in `port_table[].poe_enable` / `.up`. PoE ports only — there is no outlet equivalent. |
| `rps-port-recovery` | `port` (int, from `rps.rps_port_table[].port_idx`) | Clear that output's `port_error_disabled`, resume delivering power, report the cleared state. |
| `port-recovery` | `port` (int), `unit_id` (omitted when not stacked), `id` | Clear `port_table[<port>].error_disabled`. |
| `clear-counters` | `port` (int), `unit_id` (omitted when not stacked) | Zero that port's byte/packet/error counters and report the zeroed values. |
| `clear-all-counters` | `unit_id` (omitted when not stacked) | The same, for every port. |
| `unpair` | no parameters | Drop the pairing with the paired peer and report the new pairing state. |

Battery modules have no command family of their own. They are driven by the
generic commands aimed at the host device, validated against the reported
`vbms_table.battery_table` — a command naming a module the device never reported
is rejected before it reaches the wire. A module's locate state comes back as
`vbms_table.battery_table[].isLocating`; its upgrade progress as
`.client_update_progress`, `.client_update_result` and `.upgrade_to_firmware`.

### Gateway commands

| Command | Payload | Expected device response |
|---|---|---|
| `speed-test` | `use_alert` (bool, always present), `source_interface` (string `if!<ifname>`, only when a specific WAN is targeted), `up_duration` / `down_duration` (int seconds, only when non-negative) | Run the test and report `speedtest-status` on a later inform, echoing `source_interface` in the same `if!` form. |
| `query-dpi-stats` | `mac` (string, the client whose stats are wanted) | Report `dpi-stats` (or `dpi_stats`) on a later inform. |
| `clear-all-dpi-counters` | no parameters | Zero DPI counters and report the zeroed stats. |
| `terminate-remote-user-vpn` | `user`, `interface`, `remote_ip` — all three always present | Tear down that session. |
| `vpn-remote-disconnect` | payload **unconfirmed** | Disconnect a site-to-site VPN peer. |
| `force-update-neighbors` | `neighbors` (array) | Adopt the supplied neighbour list. |
| `pcap-start` | `duration` (int seconds), `upload_url`, `auth_token`, `target_interfaces` (array), `max_size` (bytes) | Capture on the named interfaces, capped at `max_size`, and upload to `upload_url` with `auth_token`. |
| `pcap-stop` | no parameters | End an in-flight capture. |

### Commands common to all three families

`set-locate` / `unset-locate` (optional `unit_id` for a stack member) — flash or
stop flashing the locate LED and report the top-level `locating` boolean, which
the controller reads back out of every inform. `reboot`, `upgrade` and
`setdefault` arrive as their own envelopes, described under the adoption
handshake; a `cmd`-style `reboot` with `reboot_type` and `unit_id` also exists
for rebooting one member of a stack. `send-crashlog` / `send-trace` /
`send-recovery` (no parameters) each ask for an artifact upload, answered with a
notification inform; `clear-crashlog` / `clear-trace` / `clear-recovery` follow
once the artifact has been collected. `set-etherlight-mode` (`mode`) is answered
with a top-level `etherlight_mode`. `show-lcm-tracker` (`tracker_seed`) /
`hide-lcm-tracker`, `calibrate` (`enabled`) and `mesh-halt` (`timeout`, long
seconds) round out the set.

### How a command is acknowledged

There is no generic acknowledgement field. The controller confirms a command
took effect by reading the corresponding state out of a later inform. A device
that performs the action but does not update the reported state is, from the
controller's side, identical to one that ignored the command.

| Command | What the controller reads back |
|---|---|
| `relayctl` | the selected outlets' `outlet_table[].relay_state` |
| `power-cycle` | `port_table[].up` / `.poe_enable` |
| `rps-port-recovery` | that output's `port_error_disabled`, now clear |
| `set-locate` / `unset-locate` | the top-level `locating` flag |
| `reboot` | `uptime` restarting from zero |
| `upgrade` | a changed `version` |
| `setdefault` | `default` and `_default_key` both true again, on the default key |
| `speed-test` | `speedtest-status.status_summary` plus a newer `rundate` |
| `send-crashlog` / `-trace` / `-recovery` | a notification inform whose `notif_reason` matches |
| a `setparam` carrying `system_cfg` | the pushed `cfgversion`, echoed back unchanged |

### Device-initiated informs

Besides the periodic heartbeat, a device can flag an inform with either of two
top-level booleans, both defaulting to `false`.

`inform_as_notif: true` adds `notif_reason` (string) and `notif_payload`
(object). The reason vocabulary is `cli-upgrade`, `cmd`, `cmd-upgrade`,
`cmd-provision`, `crashlog`, `event`, `recovery`, `setparam`, `trace`,
`unknown`; an unrecognised value resolves to `unknown` rather than failing. This
is how a device reports that it finished a provision or an upgrade, or that a
crashlog, trace or recovery artifact is ready — the controller dispatches on
the reason and answers with the matching follow-up.

`inform_as_request: true` adds `request_reason` (`geo-ip-update` or `unknown`)
and `request_payload` (object). A `geo-ip-update` request is answered with a
`geo_ip_update` reply; anything else gets `noop`.

## Capability bitmaps

A controller gates many features on integer bitmaps the device self-reports.
Asking for a feature the device never claimed returns a 404 — for example, BGP
config against a gateway that never reported the routing bit answers
`api.err.BgpUnsupportedDevice`. The bitmap fields that appear on the wire:

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
  LCM, a PoE class, an accelerometer) rather than software features. Two bits
  matter for power devices: `UNIFI_HW_CAP_RPS` (16, the device has a
  redundant-power-supply port) and `UNIFI_HW_CAP_OUTLET` (128, the device has
  switchable outlets). The outlet bit is ORed with the model's own
  smart-outlet features, so a model the controller already knows has outlets
  does not need to claim it.
- **`fw2_caps`** and **`fw3_caps`** — two more top-level ints alongside
  `fw_caps`, in the same style.
- **`smart_power_caps`** — a top-level int on power and UPS devices. It gates
  which configuration keys the controller generates at all, so a device that
  wants a feature configured must claim its bit: `1` NUT information access
  (gates the `nut_server` block), `2` auto power-cycle on AC recovery (gates
  `power_cycle_on_ac_recovery.*`), `4` buzzer (gates `beep.status`), `8` safe
  shutdown and power-cycle timing, `16` emergency power off (gates
  `epo.status`), `64` AC input voltage THD tolerance (gates
  `ac_input_thd.level`). Bit 32 is unused — that is the vendor's numbering, not
  a gap. A value of `223` has been observed in a public UPS payload; no public
  source decodes the bits.
- **`outlet_caps`** — **per row, inside `outlet_table[]`, not a device-level
  field.** This is the most common mistake with it. Bit `1` = the outlet has a
  switchable relay, bit `2` = the outlet meters power; those two are publicly
  documented as
  [`OutletCapability.RELAY` and `.METERING`](https://github.com/Kane610/aiounifi/blob/7154e750dd0f1c7415b9c390fb2053f251c13a92/aiounifi/models/outlet.py#L9-L14),
  and `1` and `3` are the values a USP-PDU-Pro sends for its USB and AC outlets
  respectively. Four higher bits are visible in real payloads; their meanings
  carry no public source and are stated without one. `4` — the relay is driven
  automatically by the device, and `outlet_overrides` stop being applied to that
  outlet once `vbms_table.battpool` is present. `8` — the outlet reports a
  physical toggle in `button_state`, and the controller folds all such outlets
  into the device-level `outlet_enabled` state, raising an event when it
  changes. `65536` — the outlet is AC. `131072` — the outlet is USB.
  **`outlet_caps` ≥ 65536 doubles as a version
  test**: a row at or above that value is declaring the newer encoding and the
  AC question is answered from bit 65536, while a lower value falls back to
  `outlet_type == 0`. So a row that sets bit 8 alone is still read through the
  legacy path. Real values seen in public payloads include `1`, `3`, `65539`,
  `65541` and `65549`.
- **`psu_table[].psu_caps`** — also per row, not device-level: `1` charge
  control, `2` power meter, `4` hot-swappable. The controller only ever masks
  it together with `psu_type`, to decide whether a missing PSU is an error and
  whether the device has redundant supplies.
- **`psu_table[].psu_type`** and the device-level **`power_source`** share one
  vocabulary and two masks: a power-*method* half,
  `POWER_METHOD_MASK = 0x0000FFFF` (`1` 802.3af, `2` 802.3at, `4` 802.3bt type
  3, `8` 802.3bt type 4, `16` passive 24 V), and a power-*supply* half,
  `POWER_SUPPLY_MASK = 0xFFFF0000` (`0x10000` PoE injector, `0x20000` AC,
  `0x40000` adapter, `0x80000` DC, `0x100000` RPS, `0x200000` battery, plus
  `0x10000000` auto). The controller splits a device's PSU rows into PoE
  budgets and adapter budgets on that boundary. Send `0` rather than a guess.
- **`usg_caps`** and **`usg2_caps`** — top-level ints on gateways. The
  controller does not store `usg_caps` as reported: it **ORs extra bits in**
  from four legacy booleans in the same inform before storing — `has_dpi` sets
  bit 1, `has_porta` bit 2, `has_default_route_distance` bit 4, `has_ssh_disable`
  bit 8 — and a non-zero `radius_caps` sets bit 16. A gateway can therefore
  express these four features either way.
- **`radius_caps`** — a top-level int that is never masked, only tested for
  non-zero. Any non-zero value behaves identically.

And one field whose name says bitmap and is not one:

- **`gw_caps`** is **not a bitmap.** It is a sub-document with named boolean
  keys under `gw_caps.caps`, each read by name and defaulting to `false` when
  absent. There is no bit numbering to decode, and an integer value gates
  nothing. Three keys are consumed: `wan_magic`, `slaac_detect`,
  `dhcpv6_detect`. Emit it as
  `{"gw_caps": {"caps": {"wan_magic": true, "slaac_detect": true}}}`.

Two further pitfalls worth stating. `switch_caps` is likewise not a single int
but an object of sub-bitmaps, as noted above. And the whole capability update is
dropped — storing none of `fw_caps`, `hw_caps`, `switch_caps`, `usg_caps` or
`udapi_caps` — when a device sends `udapi_caps` with an empty or missing
`udapi_version`; see the pairing rule under *Every inform, pending or adopted*.

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
