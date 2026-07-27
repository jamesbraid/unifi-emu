# Firmware extraction design

## Goal

Build a reproducible, Go-first pipeline that turns official UniFi firmware into
small, reviewable evidence about device behaviour. The first useful facts are
inform fields, command names, capability bitmap families, board identifiers,
and other strings that can improve the emulator's model profiles.

Firmware images and unpacked files remain cache inputs. They are never committed
to the repository. Every derived fact records enough provenance to find it
again in the source image.

## Approaches

### Shell together existing firmware tools

Binwalk, `dumpimage`, `lzop`, and filesystem-specific utilities cover many of
the formats in the current firmware catalogue. This is the quickest exploratory
path, but it makes results depend on host packages and tool versions.

### Make the complete reverse-engineering stack pure Go

A pure-Go implementation could eventually parse every container and filesystem,
disassemble every supported CPU, and infer higher-level behaviour. That would
be portable, but it would front-load a large amount of format and architecture
work before producing useful evidence.

### Use a Go pipeline with bounded decoders

This is the selected approach. Go owns catalogue ingestion, downloads,
deduplication, checksum verification, safe container traversal, common archive
decoding, string analysis, provenance, and deterministic output. Maintained Go
libraries provide CPIO, FIT/FDT, compression, and SquashFS support. The one
exception is lzop framing: u-root itself invokes the reference `lzop` program,
and no maintained pure-Go reader provides the same stream contract. The
dependency is explicit and absence is a normal per-artifact failure.

This gets deterministic results for the firmware families already inspected
without turning the project into a general-purpose decompiler.

## Inputs and selection

The command is `fwextract`. It supports two entry paths:

- a firmware catalogue JSON file, with optional filtering by platform and
  checksum;
- one local image with caller-supplied platform, version, and expected SHA-256.

Fetching a catalogue URL is allowed explicitly, but offline file input is the
default contract. Catalogue records are deduplicated by SHA-256 before any
download. The command never downloads the whole catalogue unless the caller
selects it deliberately.

Downloads use a caller-selected cache directory. A cached image is accepted
only after its size and SHA-256 match the catalogue. Partial and corrupt files
do not become valid cache entries.

## Extraction pipeline

The pipeline treats firmware as a graph of bounded byte ranges. Each decoder
receives a range and returns named child ranges; it does not write arbitrary
paths from the image to disk.

The initial decoder set covers the formats found in representative switch,
legacy AP, and modern AP images:

- Ubiquiti `UBNT` containers and their `PART`, `EMMC`, and trailer records;
- U-Boot legacy images;
- FIT images and their embedded payload descriptions;
- gzip, bzip2, LZ4, Zstandard, XZ, LZMA, and lzop payloads;
- `newc` CPIO archives;
- SquashFS filesystems through an `io/fs` reader;
- tar archives;
- opaque ESP32 application images, which still support whole-image string
  analysis.

Unknown or unsupported children are recorded in the manifest. They do not abort
other images in the same run.

All lengths, offsets, nesting depth, expanded byte counts, and archive paths are
validated. The implementation applies configurable limits to image size,
decompressed size, child count, and nesting depth so malformed input cannot
cause unbounded work or path traversal.

## Evidence analysis

Analysis covers every extracted regular file. Management agents such as
`usr/bin/mcad` remain especially useful, but do not suppress board data,
scripts, libraries, or kernel modules.

Printable strings are classified into a deliberately small vocabulary:

- inform and management fields, including `cfgversion`;
- capability families such as `fw_caps`, `hw_caps`, `poe_caps`, and
  `feature_caps`;
- controller commands and response handlers;
- board, product, and sysid identifiers;
- state and adoption terms.

The analyzer reports observed strings with bounded context, not guessed
semantics. Only an explicit capability-field vocabulary is classified as a
bitmap; function symbols that merely end in `_caps` remain unclassified.

Each observation records:

- source image SHA-256, platform, and firmware version;
- nested artifact path;
- absolute image offset when it is known;
- extraction stages used to reach the artifact;
- artifact SHA-256 and bounded surrounding context;
- evidence kind and value;
- confidence based on direct observation, not model-family inference.

## Output model

`firmware_evidence.json` is a deterministic manifest. Arrays and maps use stable
ordering so the same inputs produce a byte-for-byte identical file.

The top level records the schema version, generator version, limits, and one
entry per unique image. Each image contains catalogue aliases, container
structure, extracted artifacts, observations, warnings, and failures.

Evidence is intentionally separate from `model_profiles.json`. Static evidence
must not silently change emulator behaviour. A later model generation step may
consume only facts marked as verified by an exact static layout or a live
device observation. Family-level inference remains visible as inference.

The filesystem consumer boundary is a pair: a deterministic `rootfs.tar` plus
`rootfs.json`. The tar preserves directories, permission modes, symlinks,
hardlinks, and exact vendor file bytes while normalizing ordering, timestamps,
and ownership. It never contains runtime shims, metadata helpers, or modified
vendor files. The JSON records the source firmware digest, platform, version,
nested artifact path, tar digest, and entry count.

## Live validation

The pipeline can ingest sanitized output captured from a real device, including
the existing `mca-dump` surface, and attach it as a second evidence source.
Remote login, credential handling, and automated device access are out of scope.

JSON live input also preserves the complete typed path tree and exact integer
values. The repository's controller-derived capability definitions can turn
observed masks into named set bits and an unknown mask. Static and live
observations use the same vocabulary. A comparison report shows
facts that agree, facts seen only in firmware, and facts seen only at runtime.
This makes a live device an oracle without making it a build dependency.

## Package layout

The command stays thin:

```text
cmd/fwextract/
    main.go
internal/firmware/
    catalogue.go
    source.go
    decode.go
    ubnt.go
    uimage.go
    fit.go
    cpio.go
    analyze.go
    manifest.go
```

Format packages expose byte-oriented functions and typed errors. HTTP and the
filesystem sit behind small interfaces where tests need fault injection.

## Testing

Tests build small synthetic containers and archives. Vendor firmware is not
checked into testdata.

The suite covers:

- catalogue parsing, selection, and checksum deduplication;
- successful cache reuse and rejection of truncated or corrupt downloads;
- valid and malformed records for every supported container;
- decompression and expansion limits;
- CPIO path normalization and traversal rejection;
- evidence classification with exact offsets;
- deterministic JSON output;
- partial manifests when one image or child format is unsupported;
- comparison of static evidence with sanitized live observations.

End-to-end tests feed generated firmware through the public command and compare
the resulting manifest with a golden file. A manual smoke test against the three
representative official images validates the real formats without making
network access part of the normal test suite.

## Delivery boundaries

The first delivery includes the command, reusable internal package, synthetic
tests, documentation, and a reproducible smoke-test recipe. It does not include:

- firmware blobs or unpacked vendor files in Git;
- signature bypassing, firmware modification, or repacking;
- CPU emulation or general-purpose decompilation;
- automatic SSH access to devices;
- automatic mutation of emulator model profiles from unverified evidence.

Those boundaries keep the result useful for emulator development while making
the evidence auditable and safe to regenerate.
