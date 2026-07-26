#!/usr/bin/env python3
"""Turn a UniFi Network application jar into capability_bits.json.

The controller gates whole features on bitmaps a device reports in its
inform payload -- udapi_caps, switch_caps, hw_caps and friends. The bit
*names* live only in the Java device model: the UI bundle carries named
tables for every other bitmap but reads udapi_caps through a bare
minified constant, so there is nowhere else to learn that 1 << 22 means
UNIFI_UDAPI_CAP_ROUTES_BGP.

This reads them out of the jar. Classes are found by scanning the zip
for the constant-pool strings (no full extraction), then javap prints
the values -- `static final int` survives as a ConstantValue attribute
in the declaring class even though javac inlines it at every use site.

Identifiers in the jar are obfuscated (com.ubnt.data.uuvchZbWVhirD is
the device model), so nothing here may key off a class name.

Given more than one --source, every image must yield the same table or
this fails: the classic and UniFi OS Server images package the same
Network build differently (split jars versus one Spring Boot fat jar,
and obfuscated class names differ per build), so agreement between them
is worth asserting rather than assuming.

Usage: caps_to_json.py --source JAR IMAGE VERSION [--source ...] [--out FILE]
"""

import argparse
import io
import json
import re
import shutil
import subprocess
import sys
import tempfile
import zipfile
from pathlib import Path

# A capability constant. The UNIFI_ prefix is usual but not universal
# (PROAV_CAP_SNAPSHOT_UPLOAD), so it is optional -- a family we have
# never seen still comes out.
CONST_NAME = re.compile(rb"[A-Z0-9]+(?:_[A-Z0-9]+)*_CAPS?2?_[A-Z0-9_]+")
CONST_DECL = re.compile(
    r"^\s+(?:public|protected|private)?\s*static final int "
    r"([A-Z0-9_]*CAP[A-Z0-9_]*) = (-?\d+);",
    re.M,
)
FAMILY = re.compile(r"^((?:UNIFI_)?[A-Z0-9]+(?:_[A-Z0-9]+)?_CAP2?)_[A-Z0-9_]+$")
# Every method signature, not just the interesting ones: splitting only
# on has*Capability would run each body past its own Code block and into
# the methods after it, picking up their field names too.
METHOD_SIG = re.compile(r"^  \S.*\(.*\);\s*$", re.M)
ACCESSOR_NAME = re.compile(r"\b(\w*(?:Capability|Caps)\w*)\(")
# `ldc // String <field>` inside the body names the device document
# field that bitmap is read from.
CAPS_FIELD = re.compile(r"// String ([a-z0-9_]*caps[a-z0-9_]*)")

# Constant family -> accessor, the one join the jar does not spell out.
# An unmapped family is still emitted, with a null field, rather than
# dropped: a bitmap we do not understand yet is not a bitmap to lose.
FAMILY_ACCESSOR = {
    "UNIFI_UDAPI_CAP": "hasUdapiCapability",
    "UNIFI_SWITCH_CAP": "hasSwitchCapability",
    "UNIFI_SWITCH_CAP2": "hasSwitchCapability2",
    "UNIFI_SWITCH_VLAN_CAP": "hasSwitchVlanCapability",
    "UNIFI_HW_CAP": "hasHardwareCapability",
    "UNIFI_POE_CAP": "hasPoeCapability",
    "UNIFI_USG_CAP": "hasUSGCapability",
    "UNIFI_USG2_CAP": "hasUsg2Capability",
    "UNIFI_WIFI_CAP": "hasWifiCapability",
    "UNIFI_BLE_CAP": "hasBluetoothCapability",
}


def iter_classes(jar):
    """(container, name, bytes) for every class in the jar, one level deep.

    The classic image ships the application as a jar of classes; UniFi OS
    Server ships one Spring Boot fat jar with the application under
    BOOT-INF/lib/*.jar. One level of nesting covers both layouts.
    """
    with zipfile.ZipFile(jar) as outer:
        for info in outer.infolist():
            if info.filename.endswith(".class"):
                yield "", info.filename, outer.read(info.filename)
            elif info.filename.endswith(".jar"):
                nested = io.BytesIO(outer.read(info.filename))
                try:
                    with zipfile.ZipFile(nested) as inner:
                        for sub in inner.infolist():
                            if sub.filename.endswith(".class"):
                                yield (
                                    info.filename,
                                    sub.filename,
                                    inner.read(sub.filename),
                                )
                except zipfile.BadZipFile:
                    continue


def find_classes(jar):
    """Classes whose constant pool mentions a capability name."""
    return [
        (container, name, data)
        for container, name, data in iter_classes(jar)
        if CONST_NAME.search(data)
    ]


def javap(class_file, disassemble=False):
    cmd = ["javap", "-p", "-constants"]
    if disassemble:
        cmd.append("-c")
    cmd.append(str(class_file))
    out = subprocess.run(cmd, capture_output=True, text=True)
    if out.returncode != 0:
        sys.exit(f"javap failed on {class_file}:\n{out.stderr}")
    return out.stdout


def family_of(name):
    """UNIFI_UDAPI_CAP_ROUTES_BGP -> UNIFI_UDAPI_CAP (CAP2 kept distinct).

    None for a constant that is not one bit of a bitmap, such as the
    DEFAULT_*_CAPS seed values; the caller records those separately
    rather than dropping them.
    """
    m = FAMILY.match(name)
    return m.group(1) if m else None


def accessor_fields(dump):
    """has*Capability -> the device-document field it reads."""
    fields = {}
    bounds = [m.start() for m in METHOD_SIG.finditer(dump)] + [len(dump)]
    for start, end in zip(bounds, bounds[1:]):
        chunk = dump[start:end]
        name = ACCESSOR_NAME.search(chunk.split("\n", 1)[0])
        if not name:
            continue
        found = CAPS_FIELD.search(chunk)
        if found:
            fields.setdefault(name.group(1), found.group(1))
    return fields


def extract(jar):
    """(bitmaps, other_constants, source records) for one jar."""
    classes = find_classes(jar)
    if not classes:
        sys.exit(f"no capability constants in {jar}")

    bits, other, fields, sources = {}, {}, {}, []
    with tempfile.TemporaryDirectory() as tmp:
        for container, entry, data in classes:
            path = Path(tmp) / Path(entry).name
            path.write_bytes(data)
            found = CONST_DECL.findall(javap(path))
            if not found:
                continue
            for name, value in found:
                fam = family_of(name)
                if fam:
                    bits.setdefault(fam, {})[name] = int(value)
                else:
                    other[name] = int(value)
            source = {"class": entry, "constants": len(found)}
            if container:
                source["nested_in"] = container
            sources.append(source)
            # The class declaring the constants also declares the
            # accessors; -c is only worth its output size for that one.
            fields.update(accessor_fields(javap(path, disassemble=True)))

    bitmaps = {}
    for fam in sorted(bits):
        accessor = FAMILY_ACCESSOR.get(fam)
        bitmaps[fam] = {
            "accessor": accessor,
            "field": fields.get(accessor) if accessor else None,
            "bits": dict(sorted(bits[fam].items(), key=lambda kv: kv[1])),
        }
    return bitmaps, dict(sorted(other.items())), sources


def main():
    ap = argparse.ArgumentParser()
    # LOCAL is the copied-out jar to read; PATH is where it lives in the
    # image, which is what belongs in the committed file -- a host temp
    # path would change on every run.
    ap.add_argument(
        "--source",
        nargs=4,
        metavar=("LOCAL", "IMAGE", "VERSION", "PATH"),
        action="append",
        required=True,
    )
    ap.add_argument("--out", default="-")
    args = ap.parse_args()

    if not shutil.which("javap"):
        sys.exit("javap not found: install a JDK (brew install openjdk)")

    bitmaps, other = {}, {}
    seen = []
    for local, image, version, path in args.source:
        got_bitmaps, got_other, sources = extract(local)
        if not seen:
            bitmaps, other = got_bitmaps, got_other
        elif got_bitmaps != bitmaps or got_other != other:
            sys.exit(
                f"{image} disagrees with {seen[0]['image']} on the "
                "capability table; extract them separately and diff"
            )
        seen.append(
            {
                "image": image,
                "controller_version": version,
                "jar": path,
                "classes": sources,
            }
        )

    versions = sorted({s["controller_version"] for s in seen})
    doc = {
        "_source_note": (
            "generated by scripts/extract-caps.sh from the Network "
            "application jar; bit names exist nowhere else. Values are "
            "Java ints, so a bit-31 constant is negative."
        ),
        "controller_version": versions[0] if len(versions) == 1 else versions,
        "sources": seen,
        "bitmaps": bitmaps,
        # Capability constants that are not one bit of a bitmap (seed
        # defaults, mostly). Recorded so the extraction is visibly
        # complete rather than quietly filtered.
        "other_constants": other,
    }
    text = json.dumps(doc, indent=2) + "\n"
    if args.out == "-":
        sys.stdout.write(text)
    else:
        Path(args.out).write_text(text)
        total = sum(len(b["bits"]) for b in bitmaps.values())
        print(f"wrote {args.out}: {len(bitmaps)} bitmaps, {total} bits")


if __name__ == "__main__":
    main()
