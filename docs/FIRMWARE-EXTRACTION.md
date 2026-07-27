# Firmware extraction

`fwextract` turns a firmware image into evidence that can be reviewed without
committing the image or its unpacked filesystem. It is an analysis tool, not a
firmware updater or modifier.

## What it decodes

The Go pipeline currently handles:

- Ubiquiti `UBNT` containers with `PART` and `EMMC` records;
- legacy U-Boot images and FIT device trees;
- gzip, bzip2, LZ4, Zstandard, XZ, LZMA, and lzop compression;
- tar and `newc` CPIO archives;
- SquashFS 4 filesystems;
- embedded LZMA/XZ initramfs images inside Linux kernels;
- opaque ESP32 images through bounded printable-string analysis.

Every parser checks lengths before slicing. Archive paths cannot be absolute or
escape their parent. Image bytes, total expanded bytes, artifact count, nesting
depth, and LZMA dictionary size are bounded.

The expanded-byte budget is cumulative across decompressed streams and
materialized archive entries; nested and sibling archives do not reset it.

The implementation uses u-root's CPIO and device-tree packages and
`github.com/CalebQ42/squashfs` for the SquashFS `io/fs` view. Install `lzop`
when analyzing images that contain an lzop stream. u-root uses the same
reference executable strategy in its bzImage support; it does not expose a
general lzop reader. `fwextract` reports a normal decompression failure when the
executable is unavailable.

The SquashFS dependency is MIT-licensed; its default pure-Go LZO support uses
the GPLv2 `github.com/rasky/go-lzo` package. This project intentionally keeps
that support enabled so LZO-compressed SquashFS images remain readable.

## Catalogue mode

Save the release catalogue for a repeatable offline selection:

```sh
curl -fL \
  'https://fw-update.ui.com/api/firmware-latest?filter=eq~~product~~unifi-firmware&filter=eq~~channel~~release' \
  -o firmware-latest.json

go run ./cmd/fwextract -index firmware-latest.json \
  -cache tmp/firmware -platform U7PRO \
  -out tmp/u7pro-firmware-evidence.json
```

Records are deduplicated by SHA-256 before download. A cache entry is reused
only when its length and digest still match the catalogue. Use repeated
`-platform` flags, a comma-separated platform list, or `-sha256` to narrow the
selection. `-all` is deliberately explicit because the release catalogue is
large.

## Evidence and offsets

Each image records all catalogue URL aliases, digest, version, platform
aliases, decoded artifact graph, warnings, failures, and classified
observations. An artifact offset uses `offset_basis: "parent"` and is relative
to the immediately containing artifact. An observation uses
`offset_basis: "artifact"` and is relative to the named artifact. This remains
meaningful after decompression, where no honest absolute image offset exists.

Every artifact has its own SHA-256. Static observations include bounded
printable context and use `confidence: "observed_static"`. They report what is
present in bytes; a bare string never claims that a code path runs or that a
bitmap bit is enabled for a model.

## Live comparison

Pass sanitized JSON or `key=value` text from `mca-dump` with `-live`. For JSON,
the manifest retains every path, container shape, scalar type, and exact numeric
value. This avoids rounding 64-bit capability masks. It also includes three
stable comparison lists: `agreed`, `static_only`, and `live_only`.

Use `-capability-bits capability_bits.json` with JSON live evidence to decode
each observed bitmap into its controller-defined bit names and an explicit
unknown mask:

```sh
go run ./cmd/fwextract ... \
  -live tmp/u7pro-mca-dump.json \
  -capability-bits capability_bits.json
```

Input must already be sanitized. Remote access and credential handling are
intentionally outside this command.

## Evidence boundary

Firmware strings are discovery evidence, not emulator behavior. Faithful
behavior requires a matching sanitized live trace that preserves full INFORM
payloads and controller responses for factory, adopting, managed, provisioning,
and upgrading phases. Generated emulator profiles should cite both the artifact
hash/static context and the live path/value. Static-only candidates must not
change runtime behavior.

The Nyarime SquashFS and UBI readers were evaluated but are not dependencies.
Their current tagged APIs require disk extraction or omit validation and
ordering needed for trustworthy evidence. UBI/UBIFS decoding remains
unsupported, so those bytes fall through to bounded opaque analysis until a
bounded `io.ReaderAt`/walk API validates CRCs, volume layout, and logical block
ordering.

## Root filesystem consumer contract

For one selected image, `-rootfs-tar` and `-rootfs-json` publish the emulator
consumer artifacts:

```sh
go run ./cmd/fwextract ... \
  -rootfs-tar tmp/U7PRO/rootfs.tar \
  -rootfs-json tmp/U7PRO/rootfs.json
```

`rootfs.tar` is deterministic. Entries are ordered by path; timestamps and
ownership are normalized; vendor file bytes, permission modes, directories,
symlinks, and hardlink relationships are preserved. It contains no helper,
runtime shim, manifest, or modified vendor file.

`rootfs.json` is the commit marker for the pair. It contains exactly the source
firmware digest, platform, version, selected nested artifact path, tar digest,
and entry count. Consumers verify the tar digest before use and ignore a tar
that has no metadata file.

## Real-image smoke test

Given locally obtained images and the SHA-256 published in the catalogue, run
local mode for one image from each family:

```sh
go run ./cmd/fwextract -image switch.bin \
  -sha256 "$SWITCH_SHA256" -platform USM8P -version "$SWITCH_VERSION" \
  -out tmp/switch-evidence.json

go run ./cmd/fwextract -image legacy-ap.bin \
  -sha256 "$LEGACY_AP_SHA256" -platform U7MP -version "$LEGACY_AP_VERSION" \
  -out tmp/legacy-ap-evidence.json

go run ./cmd/fwextract -image modern-ap.bin \
  -sha256 "$MODERN_AP_SHA256" -platform U7PRO -version "$MODERN_AP_VERSION" \
  -out tmp/modern-ap-evidence.json
```

Check that every manifest has no failures. The switch should be analyzed as an
opaque ESP-family image. The legacy AP should traverse UBNT, uImage, LZMA, and
CPIO. The modern AP should traverse UBNT, FIT, lzop/LZO, and CPIO. Both AP
manifests should contain an artifact ending in `usr/bin/mcad`.
