#!/usr/bin/env bash
# Re-extract capability_bits.json from a UniFi controller image.
#
# The controller gates features on bitmaps the device reports (udapi_caps,
# switch_caps, hw_caps, ...). Only the Java device model names the bits, so
# refreshing the vocabulary for a new controller means going back to a jar.
# This is that step, kept runnable rather than remembered.
#
#   scripts/extract-caps.sh                     # both default images
#   OUT=x.json scripts/extract-caps.sh <image>...
#
# Needs docker and a JDK (javap). Both controller layouts work: the classic
# image splits a launcher ace.jar from lib/internal/internal-dependencies.jar,
# UniFi OS Server ships one Spring Boot fat ace.jar with the application
# nested under BOOT-INF/lib. Rather than encode either, take the largest jar
# under /usr/lib/unifi/lib -- the application dwarfs the launcher by three
# orders of magnitude -- and let the parser recurse one level.
#
# Both images are extracted by default and must agree. They package the same
# Network build differently, and obfuscation renames the device model class
# per build (uuvchZbWVhirD in one, HiimPm in the other), so agreement is the
# check that this reads the build rather than one packaging of it.
set -euo pipefail

DEFAULT_IMAGES=(
	ghcr.io/jamesbraid/unifi-os-server:5.1.21-seeded
	ghcr.io/jamesbraid/unifi-network:sim
)
OUT=${OUT:-capability_bits.json}
VERSION_FILE=/usr/lib/unifi/webapps/ROOT/app-unifi/.version

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

command -v docker >/dev/null || { echo "docker not found" >&2; exit 1; }
command -v javap >/dev/null || { echo "javap not found: install a JDK" >&2; exit 1; }

images=("$@")
[ ${#images[@]} -gt 0 ] || images=("${DEFAULT_IMAGES[@]}")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

sources=()
for i in "${!images[@]}"; do
	image=${images[$i]}
	echo "==> $image"

	version=$(docker run --rm --entrypoint /bin/sh "$image" -c "cat $VERSION_FILE 2>/dev/null" || true)
	[ -n "$version" ] || { echo "no Network version at $VERSION_FILE" >&2; exit 1; }
	echo "    Network $version"

	jar=$(docker run --rm --entrypoint /bin/sh "$image" -c \
		"find /usr/lib/unifi/lib -name '*.jar' -printf '%s %p\n' 2>/dev/null | sort -rn | head -1 | cut -d' ' -f2-")
	[ -n "$jar" ] || { echo "no jar under /usr/lib/unifi/lib" >&2; exit 1; }
	echo "    $jar"

	cid=$(docker create "$image")
	docker cp "$cid:$jar" "$work/$i.jar"
	docker rm -f "$cid" >/dev/null

	sources+=(--source "$work/$i.jar" "$image" "$version" "$jar")
done

python3 "$here/caps_to_json.py" "${sources[@]}" --out "$OUT"
