# Firmware extraction implementation plan

> Execute each task test-first. Keep vendor firmware and unpacked artifacts out
> of Git; tests must construct synthetic images.

**Goal:** Add a deterministic Go command that verifies, unpacks, and analyzes
official UniFi firmware into provenance-rich JSON evidence.

**Architecture:** A thin `cmd/fwextract` command drives `internal/firmware`.
The internal package models byte-range artifacts, recursively decodes bounded
containers, classifies management-agent strings, and writes stable manifests.
Catalogue and local-image sources converge on the same analyzer.

**Technology:** Go 1.25, standard library, `github.com/ulikunitz/xz/lzma` for
legacy LZMA streams.

---

## Task 1: Evidence model and deterministic output

**Files:**

- Create: `internal/firmware/manifest.go`
- Test: `internal/firmware/manifest_test.go`

1. Write failing tests for canonical ordering, duplicate observation removal,
   and stable indented JSON.
2. Run `go test ./internal/firmware -run Manifest` and confirm the failure.
3. Add the manifest, image, artifact, observation, warning, and failure types.
4. Implement normalization and writing.
5. Run the focused tests and commit.

## Task 2: Catalogue selection and verified image cache

**Files:**

- Create: `internal/firmware/catalogue.go`
- Create: `internal/firmware/source.go`
- Test: `internal/firmware/catalogue_test.go`
- Test: `internal/firmware/source_test.go`

1. Write failing tests using the current firmware API envelope shape.
2. Cover platform/SHA selection, SHA deduplication, aliases, cache reuse,
   checksum rejection, atomic download, and response-size limits.
3. Implement tolerant catalogue decoding and deterministic selection.
4. Implement local and HTTP sources behind `Source`, validating size and
   SHA-256 before returning bytes.
5. Run focused tests and commit.

## Task 3: Safe archive primitives

**Files:**

- Create: `internal/firmware/decode.go`
- Create: `internal/firmware/cpio.go`
- Test: `internal/firmware/cpio_test.go`

1. Write failing tests for `newc` files, alignment, malformed headers, absolute
   paths, traversal, entry limits, and expanded-size limits.
2. Add shared extraction limits and bounded artifact types.
3. Implement a byte-oriented CPIO reader that returns regular files without
   writing them to disk.
4. Run focused tests and commit.

## Task 4: Firmware container decoders

**Files:**

- Create: `internal/firmware/ubnt.go`
- Create: `internal/firmware/uimage.go`
- Create: `internal/firmware/fit.go`
- Test: `internal/firmware/ubnt_test.go`
- Test: `internal/firmware/uimage_test.go`
- Test: `internal/firmware/fit_test.go`

1. Generate minimal valid records in tests.
2. Pin offsets, names, compression metadata, checksums, malformed lengths, and
   truncated input.
3. Implement UBNT record traversal, legacy uImage parsing, and a minimal
   read-only flattened-device-tree parser for FIT `/images` data nodes.
4. Run focused tests and commit.

## Task 5: Recursive decoding and compression

**Files:**

- Modify: `internal/firmware/decode.go`
- Test: `internal/firmware/decode_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

1. Write failing nested fixtures for gzip, LZMA, tar, uImage-to-CPIO, and
   FIT-to-CPIO.
2. Pin depth and expanded-byte failures and unsupported LZO reporting.
3. Implement recursive format detection and decoding with absolute offsets
   preserved where possible.
4. Add the pure-Go LZMA dependency and leave LZO as a typed unsupported node
   unless a small maintained pure-Go decoder passes a real-image smoke test.
5. Run focused tests and commit.

## Task 6: Evidence analysis and live comparison

**Files:**

- Create: `internal/firmware/analyze.go`
- Create: `internal/firmware/live.go`
- Test: `internal/firmware/analyze_test.go`
- Test: `internal/firmware/live_test.go`

1. Write failing tests for printable-string boundaries, classification,
   offsets, likely-agent preference, and exact deduplication.
2. Write failing tests for sanitized live JSON/text ingestion and static/live
   agreement reports.
3. Implement the fixed evidence vocabulary and comparison model.
4. Run focused tests and commit.

## Task 7: End-to-end pipeline and CLI

**Files:**

- Create: `internal/firmware/pipeline.go`
- Create: `cmd/fwextract/main.go`
- Test: `internal/firmware/pipeline_test.go`
- Test: `cmd/fwextract/main_test.go`
- Modify: `README.md`

1. Write failing end-to-end tests around a synthetic UBNT image and the command
   argument contract.
2. Implement local image mode, catalogue file/URL mode, platform/SHA filters,
   cache selection, limits, and partial-failure manifests.
3. Document offline and official-catalogue examples plus the evidence safety
   boundary.
4. Run command tests and commit.

## Task 8: Real-image smoke validation

**Files:**

- No vendor firmware files added.
- Optionally create ignored output under `tmp/firmware/`.

1. Run the command against the previously verified USM8P, UAP-AC-Mesh-Pro, and
   U7PRO images when they remain available in `/tmp`.
2. Confirm hashes, top-level format recognition, extracted `mcad` where
   supported, and capability/inform string observations.
3. Record only commands and summarized results in the PR.

## Task 9: Final verification and review

1. Run `gofmt` on all added Go files.
2. Run `go test ./internal/firmware ./cmd/fwextract`.
3. Run `go test ./...`.
4. Run `go vet ./...`.
5. Run `git diff --check`.
6. Review the complete diff for safety, deterministic output, and accidental
   firmware artifacts.
7. Commit remaining documentation or review fixes, push the branch, and open a
   PR with verification receipts and known LZO limitations stated explicitly.
