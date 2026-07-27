#!/usr/bin/env python3
"""Turn a UniFi Network application jar into capability_bits.json.

The controller gates whole features on bitmaps a device reports in its
inform payload -- udapi_caps, switch_caps, fw_caps and friends. The bit
*names* live only in the Java Network application: the UI bundle carries
named tables for two of the bitmaps and reads the rest through bare
minified constants, so there is nowhere else to learn that 1 << 22 means
UNIFI_UDAPI_CAP_ROUTES_BGP.

Three different things in the jar name a bit, and an extractor that knows
only the first reports about half the vocabulary:

  1. a `static final int` constant -- UNIFI_UDAPI_CAP_ROUTES_BGP = 4194304.
     The value survives as a ConstantValue attribute in the declaring
     class even though javac inlines it at every use site.
  2. an *enum* constant -- BPDU_GUARD("BPDU_GUARD", 0, 1). There is no
     ConstantValue anywhere; the bit is a constructor argument. Because
     java.lang.Enum's constructor takes the name, the source-level name
     survives obfuscation as an `ldc` string in <clinit> even when the
     static field it lands in is called chgwykfBxZCAuEHPPQ.
  3. nothing at all -- fw_caps, fw2_caps, fw3_caps and wifi_caps2 pass
     bare literals to hasFirmwareCapability(int) and friends. The only
     surviving name is the *wrapper method* that passes each literal, so
     supportsMacBasedVlans is the sole evidence that fw_caps bit 31 is
     MAC-based VLANs. That is a paraphrase of the author's intent rather
     than the vendor's identifier, and the file grades it as such.

So a bitmap is found by bytecode SHAPE, never by name: a read of
getInt("<something>caps") whose value reaches an `iand`, and an enum whose
constants carry distinct powers of two. Identifiers are obfuscated per
build -- the device model is com.ubnt.data.uuvchZbWVhirD in one image and
com.ubnt.data.HiimPm in the other -- so no class name may decide anything,
and no class name may reach the output outside the per-image provenance.

Nothing found is dropped. A class the scan flagged and then failed to
understand is reported, a bit seen with no name is reported as a value,
and a bitmap with no proven document field is reported with a null path.
The silent `continue` is exactly how the enum bitmaps stayed missing.

Classes are read straight out of the zip and parsed in-process: never
unpacked to disk (APFS is case-insensitive and folds com/ubnt/service/az
into .../aZ, silently losing 512 classes) and never handed to javap (its
text cannot express which `iand` an expression feeds, which is the whole
question, and it is a JDK to install for facts already in the bytes).

Given more than one --source, every image must yield the same table or
this fails: the classic and UniFi OS Server images package the same
Network build differently (split jars versus one Spring Boot fat jar, and
obfuscated class names differ per build), so agreement between them is
worth asserting rather than assuming.

Usage: caps_to_json.py --source JAR IMAGE VERSION PATH [--source ...]
                       [--swai swai.js] [--out FILE]
"""

import argparse
import io
import json
import re
import struct
import sys
import zipfile
from collections import Counter, defaultdict
from pathlib import Path

# ======================================================================
# what a capability constant, field and wrapper look like
# ======================================================================

# Flags a class worth reading constants out of. The UNIFI_ prefix is usual
# but not universal (PROAV_CAP_SNAPSHOT_UPLOAD), so it is optional -- a
# family we have never seen still comes out.
CONST_NAME = re.compile(rb"[A-Z0-9]+(?:_[A-Z0-9]+)*_CAPS?2?_[A-Z0-9_]+")
# Which constants of a flagged class to keep. Requiring CAP in the name
# was too strict: it dropped UNIFI_USG2_DPI_REFRESH_INTERVAL_ENHANCEMENTS
# (a usg2_caps bit) and the whole UNIFI_ERROR_* set. Keeping *every* int
# in the class is too loose -- MAX_PORTS and UDMPRO_LAST_ETH_INDEX are not
# capabilities -- so the line is the vendor's own UNIFI_ namespace.
CONST_KEEP = re.compile(r"^(?:UNIFI_[A-Z0-9_]+|[A-Z0-9_]*CAP[A-Z0-9_]*)$")
FAMILY = re.compile(r"^((?:UNIFI_)?[A-Z0-9]+(?:_[A-Z0-9]+)?_CAP2?)_[A-Z0-9_]+$")

# A capability bitmap field in the device document. Deliberately not
# anchored on a known prefix -- discovering unlisted bitmaps is the point.
CAPS_FIELD = re.compile(r"^[a-z][a-z0-9_]*caps(?:[0-9]|_[a-z0-9_]+)?$")
LIST_FIELD = re.compile(r"^(?:config_)?([a-z0-9]+)_(?:table|overrides|list)$")

# Method-name prefixes that carry meaning through obfuscation: the
# obfuscator renames exactly the private members and keeps the public
# API, and these are the public names that name a single capability bit.
# Obfuscated names (chgwykfBxZCAuEHPPQ, jRsSex) never match. Nothing
# private ever reaches the shared table -- a per-build identifier in
# there would break the cross-image agreement that proves this read the
# build rather than one packaging of it -- so both guards apply to any
# method name that gets printed.
WRAPPER = re.compile(
    r"^(?:supports?|has|is|are|can|should|allows?|does|uses?|needs?"
    r"|requires?|enables?|includes?|with)(?=[A-Z])(.*)$"
)
STRIP_TAIL = re.compile(r"(?:Capability|Capabilities|Caps?)$")
# Cosmetic only: camel-splitting mangles embedded acronyms (QoS -> QO_S).
# Every bit keeps its provenance, so this can only make a name readable,
# never change what it is derived from.
ACRONYM = {
    "QoS": "QOS", "IPv4": "IPV4", "IPv6": "IPV6", "Ipv4": "IPV4",
    "Ipv6": "IPV6", "Https": "HTTPS", "Dnsmasq": "DNSMASQ",
    "DnsMasq": "DNSMASQ", "Sha256": "SHA256", "Sha512": "SHA512",
    "SNMPv3": "SNMPV3", "Snmpv3": "SNMPV3", "Mdns": "MDNS",
}

# Constant family -> accessor, the one join the jar does not spell out:
# UNIFI_SWITCH_CAP_* are bits of feature_caps and only a human knows it.
# Everything else is joined by evidence. An unmapped family is still
# emitted, with a null field, rather than dropped: a bitmap we do not
# understand yet is not a bitmap to lose.
FAMILY_ACCESSOR = {
    "UNIFI_SWITCH_CAP": "hasSwitchCapability",
    "UNIFI_SWITCH_CAP2": "hasSwitchCapability2",
    "UNIFI_SWITCH_VLAN_CAP": "hasSwitchVlanCapability",
}

# How a document list maps to a scope. These are wire-format names from
# the device document, not obfuscated Java identifiers, so they are stable
# across builds; an unlisted table still comes out as its own scope rather
# than being dropped.
LIST_SCOPE = {
    "port_table": "port", "port_overrides": "port", "ethernet_table": "port",
    "radio_table": "radio", "radio_table_stats": "radio", "vap_table": "vap",
    "outlet_table": "outlet", "outlet_overrides": "outlet",
    "psu_table": "psu",
}
CONTAINER_SCOPE = {"port": "port", "radio": "radio", "outlet": "outlet",
                   "sfp": "port", "psu": "psu"}

# Bit names the controller UI knows and the jar does not. Lifted from the
# minified bundle, where each bitmap is read through a curried helper --
# `k=x("fw_caps")` -- so the table that helper is called with is the table
# for that field. Re-derive for a new build with:
#   rg -o -a '[A-Za-z_$]{1,3}=x\("[a-z0-9_.]+"\)' swai.js
# then find the object literal passed to that helper. --swai re-greps the
# bundle so a stale table fails loudly instead of being asserted.
UI_TABLES = {
    "fw_caps": {1: "SSH", 2: "STA_STAT", 4: "UTERM", 8: "DNSMASQ",
                16: "QCASWITCH", 32: "FWUPDATE", 64: "DPI", 128: "LAG",
                256: "OWRTSWITCH", 8192: "SNMP", 16384: "SNMPV3",
                131072: "ELITE_DEVICE", 8388608: "HOTSPOT_2"},
    "wifi_caps2": {1: "MLO", 2: "MONITOR_RF_SCAN", 4: "GREEN_AP",
                   8: "DFS_BACKGROUND_SCAN", 32: "ROAMING_ASSISTANT",
                   128: "QUICK_SCAN", 16384: "MESH_MLO_PARENT",
                   32768: "MESH_MLO_CHILD", 262144: "WIRELESS_UPLINK_ONLY"},
}

# How much a name is worth, best first. A declared constant is a fact; a
# method name is a paraphrase; a bare literal is a number somebody has to
# name by hand, and gets no name at all rather than an invented one.
PROVENANCE_RANK = ["named-constant", "enum-constant", "enum-table",
                   "ui-bundle", "method-name"]

ACC_PUBLIC = 0x0001
ACC_PRIVATE = 0x0002
ACC_PROTECTED = 0x0004
ACC_STATIC = 0x0008
ACC_ENUM = 0x4000

CONSTANT_Utf8, CONSTANT_Integer, CONSTANT_Float = 1, 3, 4
CONSTANT_Long, CONSTANT_Double, CONSTANT_Class = 5, 6, 7
CONSTANT_String, CONSTANT_Fieldref, CONSTANT_Methodref = 8, 9, 10
CONSTANT_InterfaceMethodref, CONSTANT_NameAndType = 11, 12
CONSTANT_MethodHandle, CONSTANT_MethodType = 15, 16
CONSTANT_Dynamic, CONSTANT_InvokeDynamic = 17, 18
CONSTANT_Module, CONSTANT_Package = 19, 20


# ======================================================================
# class files
# ======================================================================


class ClassFile(object):
    """Enough of JVMS chapter 4 to read the pool, code and int constants."""

    __slots__ = ("cp", "access_flags", "this_class", "super_class",
                 "interfaces", "fields", "methods", "attributes", "name",
                 "supername", "int_fields", "_bsm")

    def utf8(self, i):
        e = self.cp[i]
        return e[1] if e and e[0] == CONSTANT_Utf8 else None

    def cls_name(self, i):
        e = self.cp[i]
        return self.utf8(e[1]) if e and e[0] == CONSTANT_Class else None

    def const_value(self, i):
        e = self.cp[i]
        if e is None:
            return None
        if e[0] == CONSTANT_String:
            return self.utf8(e[1])
        if e[0] in (CONSTANT_Integer, CONSTANT_Float, CONSTANT_Long,
                    CONSTANT_Double):
            return e[1]
        return None

    def const(self, i):
        """ldc target as ('str', s) | ('int', v) | ('?', tag)."""
        e = self.cp[i]
        if e is None:
            return ("?", None)
        if e[0] == CONSTANT_String:
            return ("str", self.utf8(e[1]))
        if e[0] == CONSTANT_Integer:
            return ("int", e[1])
        return ("?", e[0])

    def ref(self, i):
        """(owner, name, descriptor) for Field/Method/InterfaceMethod refs."""
        e = self.cp[i]
        if e is None:
            return None
        if e[0] in (CONSTANT_Dynamic, CONSTANT_InvokeDynamic):
            nt = self.cp[e[2]]
            return ("<indy>", self.utf8(nt[1]), self.utf8(nt[2]))
        if e[0] not in (CONSTANT_Fieldref, CONSTANT_Methodref,
                        CONSTANT_InterfaceMethodref):
            return None
        nt = self.cp[e[2]]
        return (self.cls_name(e[1]), self.utf8(nt[1]), self.utf8(nt[2]))

    def bootstrap_methods(self):
        if self._bsm is None:
            self._bsm = []
            for name, body in self.attributes:
                if name != "BootstrapMethods":
                    continue
                n = struct.unpack_from(">H", body, 0)[0]
                off = 2
                for _ in range(n):
                    mh, na = struct.unpack_from(">HH", body, off)
                    off += 4
                    args = list(struct.unpack_from(">%dH" % na, body, off)) \
                        if na else []
                    off += 2 * na
                    self._bsm.append((mh, args))
        return self._bsm

    def indy_impl(self, i):
        """invokedynamic pool index -> ((owner, name, desc), handle kind).

        A lambda's real work lives in a synthetic method; the arguments
        captured at the indy site bind to that method's leading
        parameters, so a bit literal captured here still reaches an
        accessor. The handle kind says whether the first captured value is
        a bound receiver -- mistaking that for parameter 0 shifts every
        argument by one and silently attributes the wrong bitmap.
        """
        e = self.cp[i]
        if not e or e[0] != CONSTANT_InvokeDynamic:
            return None
        bsms = self.bootstrap_methods()
        if e[1] >= len(bsms):
            return None
        for a in bsms[e[1]][1]:
            ce = self.cp[a]
            if ce and ce[0] == CONSTANT_MethodHandle:
                ref = self.ref(ce[2])
                if ref and ref[1] not in ("metafactory", "altMetafactory"):
                    return ref, ce[1]
        return None


def parse_class(data):
    if data[:4] != b"\xca\xfe\xba\xbe":
        raise ValueError("not a class file")
    off = 8
    count = struct.unpack_from(">H", data, off)[0]
    off += 2
    cp = [None] * count
    i = 1
    while i < count:
        tag = data[off]
        off += 1
        if tag == CONSTANT_Utf8:
            ln = struct.unpack_from(">H", data, off)[0]
            off += 2
            cp[i] = (tag, data[off:off + ln].decode("utf-8", "replace"))
            off += ln
        elif tag == CONSTANT_Integer:
            cp[i] = (tag, struct.unpack_from(">i", data, off)[0])
            off += 4
        elif tag == CONSTANT_Float:
            cp[i] = (tag, struct.unpack_from(">f", data, off)[0])
            off += 4
        elif tag == CONSTANT_Long:
            cp[i] = (tag, struct.unpack_from(">q", data, off)[0])
            off += 8
            i += 1
        elif tag == CONSTANT_Double:
            cp[i] = (tag, struct.unpack_from(">d", data, off)[0])
            off += 8
            i += 1
        elif tag in (CONSTANT_Class, CONSTANT_String, CONSTANT_MethodType,
                     CONSTANT_Module, CONSTANT_Package):
            cp[i] = (tag, struct.unpack_from(">H", data, off)[0])
            off += 2
        elif tag in (CONSTANT_Fieldref, CONSTANT_Methodref,
                     CONSTANT_NameAndType, CONSTANT_InterfaceMethodref,
                     CONSTANT_Dynamic, CONSTANT_InvokeDynamic):
            cp[i] = (tag,) + struct.unpack_from(">HH", data, off)
            off += 4
        elif tag == CONSTANT_MethodHandle:
            cp[i] = (tag,) + struct.unpack_from(">BH", data, off)
            off += 3
        else:
            raise ValueError("bad constant pool tag %d" % tag)
        i += 1

    c = ClassFile()
    c.cp = cp
    c._bsm = None
    c.access_flags, c.this_class, c.super_class = \
        struct.unpack_from(">HHH", data, off)
    off += 6
    n = struct.unpack_from(">H", data, off)[0]
    off += 2
    c.interfaces = list(struct.unpack_from(">%dH" % n, data, off)) if n else []
    off += 2 * n

    def attrs(off):
        n = struct.unpack_from(">H", data, off)[0]
        off += 2
        out = []
        for _ in range(n):
            ni, ln = struct.unpack_from(">HI", data, off)
            off += 6
            out.append((c.utf8(ni), data[off:off + ln]))
            off += ln
        return out, off

    def members(off):
        n = struct.unpack_from(">H", data, off)[0]
        off += 2
        out = []
        for _ in range(n):
            af, ni, di = struct.unpack_from(">HHH", data, off)
            off += 6
            a, off = attrs(off)
            out.append({"access": af, "name": c.utf8(ni), "desc": c.utf8(di),
                        "attrs": a})
        return out, off

    c.fields, off = members(off)
    c.methods, off = members(off)
    c.attributes, off = attrs(off)
    c.name = c.cls_name(c.this_class)
    c.supername = c.cls_name(c.super_class) if c.super_class else None
    c.int_fields = {}
    for f in c.fields:
        if f["desc"] != "I":
            continue
        for aname, body in f["attrs"]:
            if aname != "ConstantValue":
                continue
            e = c.cp[struct.unpack_from(">H", body)[0]]
            if e and e[0] == CONSTANT_Integer:
                c.int_fields[f["name"]] = e[1]
    return c


def code_of(method):
    for name, body in method["attrs"]:
        if name == "Code":
            n = struct.unpack_from(">I", body, 4)[0]
            return body[8:8 + n]
    return None


OPLEN = {}


def _init_oplen():
    for op in range(0x00, 0x10):
        OPLEN[op] = 0
    OPLEN[0x10] = 1
    OPLEN[0x11] = 2
    OPLEN[0x12] = 1
    OPLEN[0x13] = 2
    OPLEN[0x14] = 2
    for op in range(0x15, 0x1a):
        OPLEN[op] = 1
    for op in range(0x1a, 0x36):
        OPLEN[op] = 0
    for op in range(0x36, 0x3b):
        OPLEN[op] = 1
    for op in range(0x3b, 0x85):
        OPLEN[op] = 0
    OPLEN[0x84] = 2                       # iinc
    for op in range(0x85, 0x99):
        OPLEN[op] = 0
    for op in range(0x99, 0xa9):
        OPLEN[op] = 2
    OPLEN[0xa9] = 1                       # ret
    OPLEN[0xaa] = OPLEN[0xab] = None      # table/lookupswitch
    for op in range(0xac, 0xb2):
        OPLEN[op] = 0
    for op in range(0xb2, 0xb9):
        OPLEN[op] = 2
    OPLEN[0xb9] = OPLEN[0xba] = 4
    OPLEN[0xbb] = 2
    OPLEN[0xbc] = 1
    OPLEN[0xbd] = 2
    OPLEN[0xbe] = OPLEN[0xbf] = 0
    OPLEN[0xc0] = OPLEN[0xc1] = 2
    OPLEN[0xc2] = OPLEN[0xc3] = 0
    OPLEN[0xc4] = None                    # wide
    OPLEN[0xc5] = 3
    OPLEN[0xc6] = OPLEN[0xc7] = 2
    OPLEN[0xc8] = OPLEN[0xc9] = 4


_init_oplen()


def walk_code(code):
    """Yield (pc, opcode, operand_bytes) over a Code attribute."""
    pc, n = 0, len(code)
    while pc < n:
        op = code[pc]
        ln = OPLEN.get(op)
        if ln is not None:
            yield (pc, op, code[pc + 1:pc + 1 + ln])
            pc += 1 + ln
            continue
        if op == 0xc4:                                    # wide
            w = 5 if code[pc + 1] == 0x84 else 3
            yield (pc, op, code[pc + 1:pc + w])
            pc += w
        elif op == 0xaa:                                  # tableswitch
            p = pc + 1
            p += (4 - (p % 4)) % 4
            lo, hi = struct.unpack_from(">ii", code, p + 4)
            end = p + 12 + 4 * (hi - lo + 1)
            yield (pc, op, code[pc + 1:end])
            pc = end
        elif op == 0xab:                                  # lookupswitch
            p = pc + 1
            p += (4 - (p % 4)) % 4
            npairs = struct.unpack_from(">i", code, p + 4)[0]
            end = p + 8 + 8 * npairs
            yield (pc, op, code[pc + 1:end])
            pc = end
        else:
            raise ValueError("unhandled opcode 0x%02x at %d" % (op, pc))


def decode(code):
    """[(pc, opcode, immediate)] -- the same walk, operands as ints."""
    out = []
    for pc, op, operand in walk_code(code):
        if op == 0x10:
            arg = struct.unpack(">b", operand[:1])[0]
        elif op == 0x11:
            arg = struct.unpack(">h", operand[:2])[0]
        elif op in (0xc4, 0xaa, 0xab):
            arg = None                    # wide / switch: nothing to read
        elif len(operand) >= 2:
            arg = int.from_bytes(operand[:2], "big")
        elif len(operand) == 1:
            arg = operand[0]
        else:
            arg = None
        out.append((pc, op, arg))
    return out


def parse_args(desc):
    out = []
    i = desc.index("(") + 1
    while desc[i] != ")":
        start = i
        while desc[i] == "[":
            i += 1
        if desc[i] == "L":
            i = desc.index(";", i) + 1
        else:
            i += 1
        out.append(desc[start:i])
    return out


def ret_type(desc):
    return desc[desc.index(")") + 1:]


def slots(t):
    return 2 if t in ("J", "D") else 1


def nargs(desc):
    return len(parse_args(desc))


def is_public(member):
    """Public or protected -- that is, a name the obfuscator kept.

    Everything private in this jar has been renamed (the document
    wrapper's own private helpers are all called chgwykfBxZCAuEHPPQ), so
    this is the test for whether a method name means the same thing in
    the next build, and in the other image's packaging of this one.
    """
    return bool(member["access"] & (ACC_PUBLIC | ACC_PROTECTED))


def param_slots(desc, static):
    """[(local slot, type)] for the declared parameters."""
    slot, out = (0 if static else 1), []
    for t in parse_args(desc):
        out.append((slot, t))
        slot += slots(t)
    return out


# ======================================================================
# the jar
# ======================================================================

CLASS_PREFIXES = ("BOOT-INF/classes/", "WEB-INF/classes/")


def internal_name(entry):
    """Zip entry -> JVM internal class name."""
    name = entry[:-6]
    for p in CLASS_PREFIXES:
        if name.startswith(p):
            return name[len(p):]
    return name


def iter_classes(jar):
    """(container, entry, bytes) for every class, one level of nesting deep.

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
                                yield (info.filename, sub.filename,
                                       inner.read(sub.filename))
                except zipfile.BadZipFile:
                    continue


class Jar(object):
    """One image's classes, with the analysis scoped to the application.

    A fat jar carries the Network application beside Spring, Tomcat and
    Bouncy Castle. Scoping to the archive member that ships the capability
    constants keeps the two packagings comparable -- which is what makes
    agreement between them mean anything -- and keeps a third-party class
    whose pool happens to hold the bytes "caps" out of the way. The member
    is found by where the constants are, never by its name.
    """

    def __init__(self, path):
        self.entries = list(iter_classes(path))
        self.container = None
        self.blobs = {}
        self._cls, self._ev, self._callers, self._refs = {}, {}, {}, {}

    def select(self, container):
        self.container = container
        self.blobs = {}
        for cont, entry, data in self.entries:
            if cont == container:
                self.blobs.setdefault(internal_name(entry), data)

    def cls(self, name):
        """Parse and remember one class. Callers that sweep the whole jar
        use methods_of() instead: holding every parsed class would cost
        more memory than the jar itself."""
        if name not in self._cls:
            data = self.blobs.get(name)
            try:
                self._cls[name] = parse_class(data) if data else None
            except Exception:
                self._cls[name] = None
        return self._cls[name]

    def method(self, owner, name, desc):
        c = self.cls(owner)
        if not c:
            return None
        return next((m for m in c.methods
                     if m["name"] == name and m["desc"] == desc), None)

    def events(self, owner, method):
        key = (owner, method["name"], method["desc"])
        if key not in self._ev:
            c = self.cls(owner)
            self._ev[key] = Sim(c, method).run().events if c else []
        return self._ev[key]

    def refs(self, owner):
        """Classes whose bytes mention `owner` (cheap in-memory scan)."""
        if owner not in self._refs:
            needle = owner.encode()
            self._refs[owner] = sorted(n for n, d in self.blobs.items()
                                       if needle in d)
        return self._refs[owner]

    def methods_of(self, names):
        """(ClassFile, method, instructions) over the given classes."""
        for name in names:
            c = self._cls.get(name)
            if c is None:
                try:
                    c = parse_class(self.blobs[name])
                except Exception:
                    continue
            for m in c.methods:
                code = code_of(m)
                if not code:
                    continue
                try:
                    ins = decode(code)
                except Exception:
                    continue
                if ins:
                    yield c, m, ins

    def callers(self, owner, name, desc):
        """[(class, method, recv, args, container)] call sites of a method.

        `container` is set for an invokedynamic call site (a lambda or
        method reference): it is the collection the lambda is applied to,
        so the callee's uncaptured parameters are elements of it.
        """
        key = (owner, name, desc)
        if key in self._callers:
            return self._callers[key]
        out = []
        self._callers[key] = out                    # also breaks recursion
        for cname in self.refs(owner):
            c = self.cls(cname)
            if not c:
                continue
            for m in c.methods:
                if not code_of(m):
                    continue
                evs = self.events(cname, m)
                for ev in evs:
                    if ev[0] == "invoke" and (ev[2], ev[3], ev[4]) == key:
                        out.append((cname, m, ev[5], list(ev[6]), None))
                    elif ev[0] == "indy" and ev[6] == key:
                        recv, args = indy_recv_args(self, ev)
                        out.append((cname, m, recv, args, applied_to(evs, ev)))
            if len(out) > MAX_CALLERS:
                break
        return out


# ======================================================================
# engine 1: forward symbolic execution
# ======================================================================
#
# Values are tuples:
#   ('int', v) ('str', s) ('const', v) ('this',) ('arg', local, type)
#   ('new', box) ('sfield', owner, name, desc)
#   ('field', owner, name, desc, recv) ('ret', owner, name, desc, recv, args)
#   ('indy', bsm, name, desc, args, impl) ('?',)
UNK = ("?",)
TOP = ("top",)                       # second slot of a long/double

FOLD = {0x60: lambda a, b: a + b, 0x64: lambda a, b: a - b,
        0x68: lambda a, b: a * b, 0x78: lambda a, b: a << (b & 31),
        0x7a: lambda a, b: a >> (b & 31), 0x7e: lambda a, b: a & b,
        0x80: lambda a, b: a | b, 0x82: lambda a, b: a ^ b}
LONG_ARITH = {0x61, 0x63, 0x65, 0x67, 0x69, 0x6b, 0x6d, 0x6f, 0x71, 0x73,
              0x7f, 0x81, 0x83}
FLOAT_ARITH = {0x62, 0x66, 0x6a, 0x6e, 0x72}


class Sim(object):
    """Best-effort straight-line symbolic execution of one method body.

    Branches are not merged -- the stack is simply carried forward --
    which is fine for the shapes here (an enum <clinit> is branch-free and
    an accessor is a single expression). Anything it cannot follow
    degrades to ('?',) rather than to a wrong answer. Do not repurpose it
    for general dataflow without fixing that.
    """

    def __init__(self, cls, method):
        self.c = cls
        self.m = method
        self.stack = []
        self.locals = {}
        self.events = []
        i = 0
        if not method["access"] & ACC_STATIC:
            self.locals[0] = ("this",)
            i = 1
        for t in parse_args(method["desc"]):
            self.locals[i] = ("arg", i, t)
            i += slots(t)

    def push(self, v):
        self.stack.append(v)

    def pop(self, n=1):
        out = []
        for _ in range(n):
            out.append(self.stack.pop() if self.stack else UNK)
        out.reverse()
        return out

    def popt(self, t):
        if slots(t) == 2:
            self.pop(1)
        return self.pop(1)[0]

    def run(self):
        code = code_of(self.m)
        if not code:
            return self
        try:
            steps = list(walk_code(code))
        except Exception:
            return self
        for pc, op, operand in steps:
            try:
                self.step(pc, op, operand)
            except Exception:
                self.stack = []
        return self

    def step(self, pc, op, operand):
        c = self.c

        def u2():
            return int.from_bytes(operand[:2], "big")

        if op == 0x00:
            return
        if op == 0x01:
            self.push(("const", None))
            return
        if 0x02 <= op <= 0x08:                       # iconst_m1 .. iconst_5
            self.push(("int", op - 0x03))
            return
        if op in (0x09, 0x0a):                       # lconst_0/1
            self.push(("int", op - 0x09))
            self.push(TOP)
            return
        if 0x0b <= op <= 0x0d:
            self.push(("const", float(op - 0x0b)))
            return
        if op in (0x0e, 0x0f):
            self.push(("const", float(op - 0x0e)))
            self.push(TOP)
            return
        if op == 0x10:
            self.push(("int", struct.unpack(">b", operand[:1])[0]))
            return
        if op == 0x11:
            self.push(("int", struct.unpack(">h", operand[:2])[0]))
            return
        if op in (0x12, 0x13):                       # ldc / ldc_w
            v = c.const_value(operand[0] if op == 0x12 else u2())
            self.push(("str", v) if isinstance(v, str) else
                      ("int", v) if isinstance(v, int) else ("const", v))
            return
        if op == 0x14:
            self.push(("const", c.const_value(u2())))
            self.push(TOP)
            return
        if 0x15 <= op <= 0x19:                       # iload .. aload
            self.push(self.locals.get(operand[0], UNK))
            if op in (0x16, 0x18):
                self.push(TOP)
            return
        if 0x1a <= op <= 0x2d:                       # *load_<n>
            kind, n = divmod(op - 0x1a, 4)
            self.push(self.locals.get(n, UNK))
            if kind in (1, 3):
                self.push(TOP)
            return
        if 0x2e <= op <= 0x35:                       # *aload
            self.pop(2)
            self.push(UNK)
            if op in (0x2f, 0x31):
                self.push(TOP)
            return
        if 0x36 <= op <= 0x3a:                       # istore .. astore
            if op in (0x37, 0x39):
                self.pop(1)
            self.locals[operand[0]] = self.pop(1)[0]
            return
        if 0x3b <= op <= 0x4e:                       # *store_<n>
            kind, n = divmod(op - 0x3b, 4)
            if kind in (1, 3):
                self.pop(1)
            self.locals[n] = self.pop(1)[0]
            return
        if 0x4f <= op <= 0x56:                       # *astore
            self.pop(4 if op in (0x50, 0x52) else 3)
            return
        if op == 0x57:
            self.pop(1)
            return
        if op == 0x58:
            self.pop(2)
            return
        if op == 0x59:                               # dup
            v = self.pop(1)[0]
            self.push(v)
            self.push(v)
            return
        if op == 0x5a:                               # dup_x1
            a, b = self.pop(2)
            self.push(b)
            self.push(a)
            self.push(b)
            return
        if op == 0x5c:                               # dup2
            a, b = self.pop(2)
            self.push(a)
            self.push(b)
            self.push(a)
            self.push(b)
            return
        if 0x5b <= op <= 0x5e:                       # dup_x2 / dup2_x1/x2
            self.stack = []
            return
        if op == 0x5f:                               # swap
            a, b = self.pop(2)
            self.push(b)
            self.push(a)
            return
        if 0x60 <= op <= 0x83:
            self.arith(op)
            return
        if op == 0x84:                               # iinc
            return
        if 0x85 <= op <= 0x98:                       # conversions, compares
            self.pop(1)
            self.push(UNK)
            return
        if 0x99 <= op <= 0x9e or op in (0xc6, 0xc7):
            self.pop(1)
            return
        if 0x9f <= op <= 0xa6:
            self.pop(2)
            return
        if op in (0xa7, 0xc8):                       # goto
            return
        if op in (0xaa, 0xab):                       # switch
            self.pop(1)
            return
        if 0xac <= op <= 0xb1:                       # returns
            self.stack = []
            return
        if op == 0xb2:                               # getstatic
            owner, name, desc = c.ref(u2())
            self.push(("sfield", owner, name, desc))
            if slots(desc) == 2:
                self.push(TOP)
            return
        if op == 0xb3:                               # putstatic
            owner, name, desc = c.ref(u2())
            v = self.popt(desc)
            self.events.append(("putstatic", pc, owner, name, desc, v))
            return
        if op == 0xb4:                               # getfield
            owner, name, desc = c.ref(u2())
            self.push(("field", owner, name, desc, self.pop(1)[0]))
            if slots(desc) == 2:
                self.push(TOP)
            return
        if op == 0xb5:                               # putfield
            owner, name, desc = c.ref(u2())
            self.popt(desc)
            self.pop(1)
            return
        if 0xb6 <= op <= 0xb9:                       # invoke*
            owner, name, desc = c.ref(u2())
            args = [self.popt(t) for t in reversed(parse_args(desc))]
            args.reverse()
            recv = None if op == 0xb8 else self.pop(1)[0]
            if name == "<init>" and isinstance(recv, tuple) \
                    and recv[0] == "new":
                recv[1]["args"] = args
                recv[1]["ctor"] = desc
            self.events.append(("invoke", pc, owner, name, desc, recv,
                                tuple(args)))
            if ret_type(desc) != "V":
                self.push(("ret", owner, name, desc, recv, tuple(args)))
                if slots(ret_type(desc)) == 2:
                    self.push(TOP)
            return
        if op == 0xba:                               # invokedynamic
            idx = u2()
            e = c.cp[idx]
            nt = c.cp[e[2]]
            name, desc = c.utf8(nt[1]), c.utf8(nt[2])
            args = [self.popt(t) for t in reversed(parse_args(desc))]
            args.reverse()
            impl = c.indy_impl(idx)
            self.events.append(("indy", pc, e[1], name, desc, tuple(args),
                                impl[0] if impl else None))
            if ret_type(desc) != "V":
                self.push(("indy", e[1], name, desc, tuple(args),
                           impl[0] if impl else None))
            return
        if op == 0xbb:                               # new
            self.push(("new", {"class": c.cls_name(u2()), "args": None}))
            return
        if op in (0xbc, 0xbd, 0xbe):                 # newarray, arraylength
            self.pop(1)
            self.push(UNK)
            return
        if op == 0xbf:                               # athrow
            self.stack = []
            return
        if op == 0xc0:                               # checkcast
            return
        if op == 0xc1:                               # instanceof
            self.pop(1)
            self.push(UNK)
            return
        if op in (0xc2, 0xc3):                       # monitorenter/exit
            self.pop(1)
            return
        if op == 0xc4:                               # wide
            sub = operand[0]
            if sub == 0x84:
                return
            n = int.from_bytes(operand[1:3], "big")
            if 0x15 <= sub <= 0x19:
                self.push(self.locals.get(n, UNK))
                if sub in (0x16, 0x18):
                    self.push(TOP)
            elif 0x36 <= sub <= 0x3a:
                if sub in (0x37, 0x39):
                    self.pop(1)
                self.locals[n] = self.pop(1)[0]
            return
        if op == 0xc5:                               # multianewarray
            self.pop(operand[2])
            self.push(UNK)
            return
        self.stack = []

    def arith(self, op):
        if op == 0x74:                               # ineg
            v = self.pop(1)[0]
            self.push(("int", -v[1]) if v[0] == "int" else UNK)
            return
        if op in (0x75, 0x77):                       # lneg / dneg
            return
        if op == 0x76:                               # fneg
            self.pop(1)
            self.push(UNK)
            return
        if op in LONG_ARITH:
            self.pop(4)
            self.push(UNK)
            self.push(TOP)
            return
        if op in (0x79, 0x7b, 0x7d):                 # lshl / lshr / lushr
            self.pop(3)
            self.push(UNK)
            self.push(TOP)
            return
        if op in FLOAT_ARITH:
            self.pop(2)
            self.push(UNK)
            return
        a, b = self.pop(2)
        f = FOLD.get(op)
        if f and a[0] == "int" and b[0] == "int":
            self.push(("int", f(a[1], b[1])))
        else:
            self.events.append(("binop", op, a, b))
            self.push(UNK)


# ======================================================================
# engine 2: backward expression rebuilding
# ======================================================================
#
# The forward simulator answers "what did this method do"; the backward
# rebuilder answers "what is this one operand made of", which is what an
# `iand` site needs. Values are
#   ('int', n) ('str', s) ('local', kind, slot) ('sfield', ref)
#   ('field', ref) ('call', ref, [args]) ('spill', kind, slot, value)
#   ('?', None)

UNKNOWN = ("?", None)
INVOKE = {0xb6, 0xb7, 0xb8, 0xb9, 0xba}
STATIC_INVOKE = {0xb8, 0xba}
IAND, IOR, PUTSTATIC = 0x7e, 0x80, 0xb3
_LOAD1 = {0x15: "i", 0x16: "l", 0x17: "f", 0x18: "d", 0x19: "a"}
_LOADN = {0x1a: "i", 0x1e: "l", 0x22: "f", 0x26: "d", 0x2a: "a"}
_STORE1 = {0x36: "i", 0x37: "l", 0x38: "f", 0x39: "d", 0x3a: "a"}
_STOREN = {0x3b: "i", 0x3f: "l", 0x43: "f", 0x47: "d", 0x4b: "a"}


def store_slot(op, arg):
    if op in _STORE1:
        return _STORE1[op], arg
    if 0x3b <= op <= 0x4e:
        base = op - (op - 0x3b) % 4
        return _STOREN[base], op - base
    return None


def _spill(cf, ins, j, kind, slot, depth):
    """Value most recently stored into a local before index j.

    Straight-line only. javac spills `int caps = doc.getInt("fw3_caps")`
    into a local before masking it, and following that spill is the
    difference between seeing a bitmap and missing it. A store made under
    a branch would be mis-attributed, so treat this as evidence.
    """
    if depth > 3:
        return None
    for k in range(j, -1, -1):
        if store_slot(ins[k][1], ins[k][2]) == (kind, slot):
            v, _ = value_at(cf, ins, k - 1, depth + 1)
            return v
    return None


def value_at(cf, ins, j, depth=0):
    """Rebuild the expression whose value is pushed by instruction j.

    Walks backwards consuming each instruction's own operands, so a nested
    call tree comes back intact. Unmodelled opcodes degrade to ('?', None)
    -- notably dup/new/aastore, so an argument built by an inline object
    construction comes back only partly resolved.
    """
    if j < 0:
        return UNKNOWN, -1
    _, op, arg = ins[j]
    if 0x02 <= op <= 0x08:
        return ("int", op - 0x03), j - 1
    if op in (0x10, 0x11):
        return ("int", arg), j - 1
    if op in (0x12, 0x13):
        return cf.const(arg), j - 1
    if op in _LOAD1 or 0x1a <= op <= 0x2d:
        if op in _LOAD1:
            kind, slot = _LOAD1[op], arg
        else:
            base = op - (op - 0x1a) % 4
            kind, slot = _LOADN[base], op - base
        got = _spill(cf, ins, j - 1, kind, slot, depth)
        if got is not None:
            return ("spill", kind, slot, got), j - 1
        return ("local", kind, slot), j - 1
    if op == 0xb2:
        return ("sfield", cf.ref(arg)), j - 1
    if op == 0xb4:
        _, k = value_at(cf, ins, j - 1, depth)
        return ("field", cf.ref(arg)), k
    if op in INVOKE:
        ref = cf.ref(arg)
        if ref is None:
            return UNKNOWN, j - 1
        want = nargs(ref[2]) + (0 if op in STATIC_INVOKE else 1)
        args, k = [], j - 1
        for _ in range(want):
            v, k = value_at(cf, ins, k, depth)
            args.insert(0, v)
        return ("call", ref, args), k
    if op == 0xc0:                                        # checkcast
        return value_at(cf, ins, j - 1, depth)
    return UNKNOWN, j - 1


def invoke_operands(cf, ins, i):
    """(receiver, [declared args]) of the invoke at index i."""
    ref = cf.ref(ins[i][2])
    args, k = [], i - 1
    for _ in range(nargs(ref[2])):
        v, k = value_at(cf, ins, k)
        args.insert(0, v)
    recv = UNKNOWN
    if ins[i][1] not in STATIC_INVOKE:
        recv, _ = value_at(cf, ins, k)
    return recv, args


def unspill(v):
    while v and v[0] == "spill":
        v = v[3]
    return v


# ======================================================================
# pass 1: the constants the jar declares
# ======================================================================


def family_of(name):
    """UNIFI_UDAPI_CAP_ROUTES_BGP -> UNIFI_UDAPI_CAP (CAP2 kept distinct).

    None for a constant that is not one bit of a named family, such as the
    DEFAULT_*_CAPS seeds or UNIFI_USG2_DPI_REFRESH_INTERVAL_ENHANCEMENTS;
    the caller places or records those rather than dropping them.
    """
    m = FAMILY.match(name)
    return m.group(1) if m else None


def is_bit(v):
    """One bit of a bitmap. Bit 31 arrives as a negative Java int."""
    return (v > 0 and (v & (v - 1)) == 0) or v == -2147483648


def unsigned(v):
    """Sort key: bit 31 is a negative Java int but the highest bit."""
    return v if v >= 0 else v + (1 << 32)


def scan_constants(jar):
    """(families, loose constants, per-class record, flagged-but-empty).

    Every class the name scan flagged is accounted for: one that yields no
    constant is reported, because a class flagged and then dropped without
    a word is exactly how the enum bitmaps stayed missing for so long.
    """
    families, loose, records, empty = defaultdict(dict), {}, [], []
    for container, entry, data in jar.entries:
        if not CONST_NAME.search(data):
            continue
        try:
            c = parse_class(data)
        except Exception as err:
            empty.append({"class": entry, "container": container,
                          "why": "unparsable: %s" % err})
            continue
        kept = {n: v for n, v in c.int_fields.items() if CONST_KEEP.match(n)}
        if not kept:
            empty.append({"class": entry, "container": container,
                          "why": "no int constants: the bit values here are "
                                 "not ConstantValue attributes"})
            continue
        for name in sorted(kept):
            fam = family_of(name)
            if fam:
                families[fam][name] = kept[name]
            else:
                loose[name] = kept[name]
        records.append({"class": entry, "container": container,
                        "constants": len(kept)})
    return dict(families), loose, records, empty


def loose_groups(loose):
    """Group loose constants under the longest shared prefix that reads
    like a bitmap: two or more members, all distinct powers of two.

    UNIFI_ERROR_{BETA_FW,OVERHEATING,FAN_ISSUE,PSU_ISSUE} = 1,2,4,8 is a
    bitmap that names no family the way UNIFI_*_CAP_* does. A lone
    constant is its own group so it can still be placed on a bitmap.
    """
    by_prefix = defaultdict(dict)
    for name, value in loose.items():
        parts = name.split("_")
        for n in range(2, len(parts)):
            by_prefix["_".join(parts[:n])][name] = value
    groups, claimed = {}, set()
    for prefix in sorted(by_prefix, key=lambda p: (-len(p.split("_")), p)):
        members = by_prefix[prefix]
        if len(members) < 2 or set(members) & claimed:
            continue
        values = sorted(members.values())
        if len(set(values)) != len(values) or not all(is_bit(v)
                                                      for v in values):
            continue
        groups[prefix] = dict(members)
        claimed |= set(members)
    for name, value in sorted(loose.items()):
        if name not in claimed:
            groups[name] = {name: value}
    return groups


def tokens_of(name):
    """A camel- or snake-cased identifier -> its upper-case word set."""
    s = re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", name)
    return {t for t in s.upper().split("_") if t}


def stem_of(prefix):
    """UNIFI_ERROR -> ERROR: the vendor prefix carries no information."""
    return prefix[len("UNIFI_"):] if prefix.startswith("UNIFI_") else prefix


# ======================================================================
# pass 2: enum bitmaps
# ======================================================================
#
# Several bitmaps are Java enums carrying the bit in the constructor.
# javap -constants prints no value for them, so the old extractor located
# the class and then dropped it. Detection is by shape: the class extends
# java/lang/Enum, <clinit> builds each constant through a constructor
# whose first two arguments are (String name, int ordinal) -- the
# java.lang.Enum contract, and a strong guard -- and some later int
# argument holds distinct powers of two. Names come from the ldc string
# handed to Enum's constructor, which is why they survive obfuscation.

MAX_DEPTH = 6
MAX_CALLERS = 12
MAX_CANDIDATES = 6

INT_GETTERS = {"getInt", "optInt", "getInteger", "getLong", "optLong"}
# stream / Optional plumbing to look through when chasing a container
PIPE = {("java/util/List", "stream"), ("java/util/Collection", "stream"),
        ("java/util/Set", "stream"),
        ("java/util/stream/Stream", "filter"),
        ("java/util/stream/Stream", "findFirst"),
        ("java/util/stream/Stream", "findAny"),
        ("java/util/stream/Stream", "map"),
        ("java/util/stream/Stream", "flatMap"),
        ("java/util/stream/Stream", "sorted"),
        ("java/util/stream/Stream", "peek"),
        ("java/util/Optional", "map"), ("java/util/Optional", "filter"),
        ("java/util/Optional", "get"), ("java/util/Optional", "orElse"),
        ("java/util/Optional", "orElseGet"), ("java/util/Optional", "flatMap")}
FINDERS = {"findOne", "findFirst", "find", "getOne"}
LIST_TYPES = ("Ljava/util/List;", "Ljava/util/Collection;", "Ljava/util/Set;")
CONTAINER_TYPES = LIST_TYPES + ("Ljava/util/stream/Stream;",
                                "Ljava/util/Optional;")
CONTAINERS = ("java/util/List", "java/util/Collection", "java/util/Set",
              "java/util/ArrayList", "java/util/LinkedList")


def enum_constants(c):
    """[(field, name, ordinal, [(argtype, intvalue|None), ...])] from <clinit>.

    `name` is the ldc string given to the java.lang.Enum constructor, so
    it is the real source-level name even when `field` is obfuscated.
    """
    clinit = next((m for m in c.methods if m["name"] == "<clinit>"), None)
    if clinit is None or not code_of(clinit):
        return []
    sim = Sim(c, clinit).run()
    field_of = {}
    for ev in sim.events:
        if ev[0] == "putstatic" and ev[2] == c.name:
            v = ev[5]
            if isinstance(v, tuple) and v[0] == "new" and v[1].get("args"):
                field_of[id(v[1])] = ev[3]
    out = []
    for ev in sim.events:
        if ev[0] != "invoke" or ev[3] != "<init>" or ev[2] != c.name:
            continue
        args, desc = ev[6], ev[4]
        argtypes = parse_args(desc)
        if argtypes[:2] != ["Ljava/lang/String;", "I"]:
            continue
        if len(args) < 2 or args[0][0] != "str" or args[1][0] != "int":
            continue
        box = ev[5][1] if isinstance(ev[5], tuple) and ev[5][0] == "new" \
            else None
        extras = [(t, a[1] if a[0] == "int" else None)
                  for t, a in zip(argtypes[2:], args[2:])]
        out.append((field_of.get(id(box)), args[0][1], args[1][1], extras))
    return out


def value_accessor(c, pos):
    """Name of the ()I getter returning ctor argument `pos`."""
    ctor = next((m for m in c.methods if m["name"] == "<init>" and
                 parse_args(m["desc"])[:2] == ["Ljava/lang/String;", "I"]),
                None)
    if ctor is None or not code_of(ctor):
        return None
    local = 3                                   # this, name, ordinal
    for t in parse_args(ctor["desc"])[2:2 + pos]:
        local += slots(t)
    field, pending = None, None
    for pc, op, operand in walk_code(code_of(ctor)):
        if 0x15 <= op <= 0x19:
            pending = operand[0]
        elif 0x1a <= op <= 0x2d:
            pending = (op - 0x1a) % 4
        elif op == 0xb5 and pending == local:   # putfield
            field = c.ref(int.from_bytes(operand[:2], "big"))[1]
            break
        else:
            pending = None
    if field is None:
        return None
    for m in c.methods:
        if m["desc"] != "()I" or not code_of(m):
            continue
        for pc, op, operand in walk_code(code_of(m)):
            if op == 0xb4:                      # getfield
                if c.ref(int.from_bytes(operand[:2], "big"))[1] == field:
                    return m["name"]
    return None


def find_bitmask_enums(jar):
    """Every bitmask-shaped enum in the analysed classes.

    Loose on purpose: a single-bit enum (VLAN_TAGGING = 1) cannot be told
    from a plain code enum by shape, and tightening the test would lose
    four real bitmaps. Whether a consumer masks the value against a
    document field is the discriminator, and that is the next pass.
    """
    out = []
    for name in sorted(jar.blobs):
        data = jar.blobs[name]
        if b"java/lang/Enum" not in data:          # cheap pre-filter
            continue
        c = jar.cls(name)
        if c is None:
            continue
        if c.supername != "java/lang/Enum" and not c.access_flags & ACC_ENUM:
            continue
        consts = enum_constants(c)
        if not consts:
            continue
        # the java.lang.Enum contract: ordinals are exactly 0..n-1. If
        # they are not, we misread the constructor, so do not guess.
        if sorted(k[2] for k in consts) != list(range(len(consts))):
            continue
        nextra = min(len(k[3]) for k in consts)
        for pos in range(nextra):
            if any(k[3][pos][0] != "I" or k[3][pos][1] is None
                   for k in consts):
                continue
            vals = [k[3][pos][1] for k in consts]
            bits = [v for v in vals if v != 0]     # 0 allowed, not a bit
            if not bits or len(set(bits)) != len(bits):
                continue
            if not all(v > 0 and (v & (v - 1)) == 0 for v in bits):
                continue
            mask = 0
            for v in bits:
                mask |= v
            out.append({
                "class": c.name,
                "arg_pos": pos,
                "accessor": value_accessor(c, pos),
                "mask": mask,
                "bits": [{"name": k[1], "value": k[3][pos][1], "field": k[0],
                          "ordinal": k[2]} for k in consts],
            })
    return out


def indy_recv_args(jar, ev):
    """(receiver, args) for a lambda call site.

    A bound instance-method reference (`this::test`) captures the receiver
    as its first captured argument -- treating that as parameter 0 shifts
    every argument by one and silently attributes the wrong value.
    """
    impl = ev[6]
    captured = list(ev[5])
    if impl is None:
        return (None, captured)
    m = jar.method(*impl)
    if m is not None and not m["access"] & ACC_STATIC and captured:
        return (captured[0], captured[1:])
    return (None, captured)


def mk_indy(ev):
    """The stack value an indy event pushed."""
    return ("indy", ev[2], ev[3], ev[4], tuple(ev[5]), ev[6])


def applied_to(events, indy_ev):
    """Receiver of the call that consumes a lambda (the stream/optional).

    Only a real collection counts: a lambda handed to some bespoke helper
    tells us nothing about where its arguments come from.
    """
    val = mk_indy(indy_ev)
    for other in events:
        if other[0] == "invoke" and any(a == val for a in other[6]):
            recv = other[5]
            if value_type(recv) in CONTAINER_TYPES:
                return recv
            return None
    return None


class Frame(object):
    """One activation: which method, and how we got into it."""

    def __init__(self, owner, method, parent=None, recv=None, args=(),
                 sam_source=None):
        self.owner = owner
        self.method = method
        self.parent = parent
        self.recv = recv                 # receiver value, in the parent frame
        self.args = list(args)           # call-site args, in the parent frame
        self.sam_source = sam_source     # (container, frame) for a lambda

    @property
    def key(self):
        return (self.owner, self.method["name"], self.method["desc"])

    def local_of_arg(self, i):
        n = 0 if self.method["access"] & ACC_STATIC else 1
        for t in parse_args(self.method["desc"])[:i]:
            n += slots(t)
        return n

    def arg_of_local(self, local):
        n = 0 if self.method["access"] & ACC_STATIC else 1
        for i, t in enumerate(parse_args(self.method["desc"])):
            if n == local:
                return i
            n += slots(t)
        return None


def value_type(v):
    """Declared type of a symbolic value, or None when unknown."""
    if not isinstance(v, tuple):
        return None
    if v[0] == "arg":
        return v[2]
    if v[0] == "ret":
        return ret_type(v[3])
    if v[0] in ("sfield", "field"):
        return v[3]
    if v[0] == "new":
        return "L%s;" % v[1]["class"]
    if v[0] == "str":
        return "Ljava/lang/String;"
    if v[0] == "int":
        return "I"
    return None


def assignable(jar, want, got):
    """Could a value of type `got` be passed where `want` is declared?

    A cheap type check, but types are ground truth in the class file, and
    it is what stops a mis-simulated call site from feeding the device
    object in where a List of psu rows belongs.
    """
    if not want or not got or want == got:
        return True
    if not (want.startswith("L") and got.startswith("L")):
        return want == got
    w, g = want[1:-1], got[1:-1]
    if w == "java/lang/Object" or g == "java/lang/Object":
        return True
    if w in CONTAINERS or g in CONTAINERS:
        return w in CONTAINERS and g in CONTAINERS
    for a, b in ((w, g), (g, w)):
        cur, n = jar.cls(a), 0
        while cur is not None and n < 6:
            if cur.name == b:
                return True
            cur, n = (jar.cls(cur.supername) if cur.supername else None), n + 1
    # interfaces and JDK types are not in the hierarchy walk above, so
    # stay permissive rather than dropping a real call site
    return jar.cls(w) is None or jar.cls(g) is None


def lift(jar, frame, local, depth):
    """Yield (value, frame) for the caller-side value of one local."""
    i = frame.arg_of_local(local)
    if i is None:
        return
    want = parse_args(frame.method["desc"])[i]
    if frame.parent is not None:
        if i < len(frame.args):
            if assignable(jar, want, value_type(frame.args[i])):
                yield (frame.args[i], frame.parent)
        elif frame.sam_source is not None:
            yield frame.sam_source
        return
    if depth >= MAX_DEPTH:
        return
    o, n, d = frame.key
    for cowner, cmethod, recv, args, container in jar.callers(o, n, d):
        if i < len(args):
            if assignable(jar, want, value_type(args[i])):
                yield (args[i], Frame(cowner, cmethod))
        elif container is not None:
            # an uncaptured lambda parameter is an element of `container`
            yield (container, Frame(cowner, cmethod))


def str_arg(args):
    for a in args:
        if isinstance(a, tuple) and a[0] == "str":
            return a[1]
    return None


def int_arg(args):
    for a in args:
        if isinstance(a, tuple) and a[0] == "int":
            return a[1]
    return None


def key_of_getter(jar, owner, name, desc):
    """Document key a no-arg accessor reads: its first string constant."""
    m = jar.method(owner, name, desc)
    if not m:
        return None
    for ev in jar.events(owner, m):
        if ev[0] == "invoke":
            s = str_arg(ev[6])
            if s is not None:
                return s
    return None


def unwrap_pipe(v):
    """Strip stream/Optional plumbing down to the underlying container."""
    for _ in range(12):
        if not (isinstance(v, tuple) and v[0] == "ret"):
            return v
        if (v[1], v[2]) in PIPE:
            v = v[4]
            continue
        if v[2] in FINDERS:
            src = next((a for a in v[5] if isinstance(a, tuple)
                        and a[0] == "ret" and ret_type(a[3]) in LIST_TYPES),
                       None)
            return src if src is not None else v
        return v
    return v


def is_typed_doc(jar, cname, doc_class):
    """A *named* wrapper for a whole document, e.g. the device model.

    The generic document class is ambiguous -- an instance can be a port
    row or a psu row -- but a subclass of it is a typed root document.
    """
    if cname == doc_class:
        return True
    c = jar.cls(cname)
    return bool(c and c.supername == doc_class)


def join_path(base, key):
    return key if not base else base + "." + key


def path_of(jar, v, frame, doc_class, depth=0):
    """Candidate (dotted path, root class) pairs for a document object.

    A list, not a single answer: when a document arrives as a parameter
    and the method has several callers, each caller is a candidate.
    Reporting all of them beats silently believing the first.
    """
    if depth > MAX_DEPTH or not isinstance(v, tuple):
        return []
    if v[0] == "this":
        if is_typed_doc(jar, frame.owner, doc_class):
            return [("", frame.owner)]
        if frame.parent is not None and frame.recv is not None:
            return path_of(jar, frame.recv, frame.parent, doc_class, depth + 1)
        if frame.sam_source is not None:
            src, srcframe = frame.sam_source
            return path_of(jar, src, srcframe, doc_class, depth + 1)
        return []
    if v[0] == "arg":
        if v[1] == 0 and not frame.method["access"] & ACC_STATIC:
            return path_of(jar, ("this",), frame, doc_class, depth + 1)
        t = v[2][1:-1] if len(v) > 2 and v[2].startswith("L") else None
        if t and t != doc_class and is_typed_doc(jar, t, doc_class):
            return [("", t)]
        out = []
        for up, upframe in lift(jar, frame, v[1], depth):
            for got in path_of(jar, up, upframe, doc_class, depth + 1):
                if got not in out:
                    out.append(got)
            if len(out) >= MAX_CANDIDATES:
                break
        return out
    if v[0] == "ret":
        _, owner, name, desc, recv, args = v
        u = unwrap_pipe(v)
        if u is not v:
            return path_of(jar, u, frame, doc_class, depth + 1)
        if recv is None:
            return []
        bases = path_of(jar, recv, frame, doc_class, depth + 1)
        if not bases:
            return []
        key = str_arg(args)
        if key is None and not args:
            key = key_of_getter(jar, owner, name, desc)
        rt = ret_type(desc)
        typed = rt.startswith("L") and rt[1:-1] != doc_class \
            and is_typed_doc(jar, rt[1:-1], doc_class)
        if key is None:
            # a builder-ish method returning the same typed document is a
            # pass-through; an unnameable sub-document or list is not --
            # do not silently promote it to the root, which is how
            # psu_table[].psu_caps once became a phantom top-level bitmap
            return bases if typed else []
        if rt == "L%s;" % doc_class or typed:
            return [(join_path(b, key), root) for b, root in bases]
        if rt in LIST_TYPES:
            return [(join_path(b, key + "[]"), root) for b, root in bases]
        return []
    return []


def scope_of(path):
    """Scope falls out of the path: no list component means per-device."""
    tables = [p[:-2] for p in path.split(".") if p.endswith("[]")]
    if not tables:
        return "device"
    last = tables[-1]
    return LIST_SCOPE.get(last,
                          re.sub(r"_(?:table|overrides|list)$", "", last))


def is_enum_value(v, ecls, acc):
    """`v` is <ecls>.SOMECONST.<accessor>()."""
    return (isinstance(v, tuple) and v[0] == "ret" and v[1] == ecls
            and (acc is None or v[2] == acc) and ret_type(v[3]) == "I")


def enum_const_of(v):
    r = v[4]
    return r[2] if isinstance(r, tuple) and r[0] == "sfield" else None


def callee_of(jar, ev):
    """((owner, method), args, recv) for an invoke or a lambda indy."""
    if ev[0] == "invoke":
        m = jar.method(ev[2], ev[3], ev[4])
        return ((ev[2], m), list(ev[6]), ev[5]) if m else (None, [], None)
    impl = ev[6]
    if impl is None:
        return (None, [], None)
    m = jar.method(*impl)
    if m is None:
        return (None, [], None)
    recv, args = indy_recv_args(jar, ev)
    return ((impl[0], m), args, recv)


def enum_bit_test_arg(jar, ecls, name, desc):
    """Index of the int argument an enum method masks with its own bit.

    The mirror image of the usual shape: instead of `caps & E.C.value()`
    the code says `E.C.matches(caps)` and the AND lives inside the enum.
    """
    m = jar.method(ecls, name, desc)
    if m is None or m["access"] & ACC_STATIC or not code_of(m):
        return None
    frame = Frame(ecls, m)
    for ev in jar.events(ecls, m):
        if ev[0] != "binop" or ev[1] not in (IAND, IOR):
            continue
        a, b = ev[2], ev[3]
        for x, y in ((a, b), (b, a)):
            own = (isinstance(x, tuple) and x[0] == "field" and x[1] == ecls
                   and x[3] == "I" and x[4] == ("this",))
            if own and isinstance(y, tuple) and y[0] == "arg":
                return frame.arg_of_local(y[1])
    return None


def resolve_caps_int(jar, v, frame, depth=0):
    """`v` holds the capability integer: find the getInt() behind it.

    Returns [(field, default, receiver_value, frame)].
    """
    if depth > MAX_DEPTH or not isinstance(v, tuple):
        return []
    if v[0] == "ret" and v[2] in INT_GETTERS and str_arg(v[5]) is not None:
        return [(str_arg(v[5]), int_arg(v[5]), v[4], frame)]
    if v[0] == "arg":
        out = []
        for up, upframe in lift(jar, frame, v[1], depth):
            out.extend(resolve_caps_int(jar, up, upframe, depth + 1))
        return out
    if v[0] == "ret":
        # a thin int-returning wrapper around the read: look inside it
        m = jar.method(v[1], v[2], v[3])
        if m is not None and code_of(m):
            sub = Frame(v[1], m, parent=frame, recv=v[4], args=list(v[5]))
            for ev in jar.events(sub.owner, sub.method):
                if ev[0] == "invoke" and ev[3] in INT_GETTERS \
                        and str_arg(ev[6]) is not None:
                    return [(str_arg(ev[6]), int_arg(ev[6]), ev[5], sub)]
    return []


def follow_mask(jar, frame, local, depth=0, seen=None):
    """Find what the bit value in `local` is AND-ed against."""
    seen = set() if seen is None else seen
    k = (frame.key, local)
    if depth > MAX_DEPTH or k in seen:
        return []
    seen.add(k)
    out = []

    def is_it(v):
        return isinstance(v, tuple) and v[0] == "arg" and v[1] == local

    for ev in jar.events(frame.owner, frame.method):
        if ev[0] == "binop" and ev[1] in (IAND, IOR):
            for x, y in ((ev[2], ev[3]), (ev[3], ev[2])):
                if is_it(x):
                    out.extend(resolve_caps_int(jar, y, frame, depth))
        elif ev[0] in ("invoke", "indy"):
            callee, args, recv = callee_of(jar, ev)
            if callee is None:
                continue
            for i, a in enumerate(args):
                if not is_it(a):
                    continue
                sam = None
                if ev[0] == "indy":
                    c = applied_to(jar.events(frame.owner, frame.method), ev)
                    sam = (c, frame) if c is not None else None
                sub = Frame(callee[0], callee[1], parent=frame, recv=recv,
                            args=args, sam_source=sam)
                out.extend(follow_mask(jar, sub, sub.local_of_arg(i),
                                       depth + 1, seen))
    return out


def attribute_enum(jar, bitmap, doc_class, device_class):
    """Locate the document field(s) an enum bitmap is read from.

    Four call shapes appear in this jar and all four are followed:
    this.hasStpCapability(E.C.value()); a static bit test whose caps
    argument is only visible by walking up to the callers; a lambda body
    reached through BootstrapMethods; and the inverted E.C.matches(caps),
    where the AND lives inside the enum.
    """
    ecls, acc = bitmap["class"], bitmap["accessor"]
    hits = {}
    for cname in jar.refs(ecls):
        if cname == ecls:
            continue
        c = jar.cls(cname)
        if not c:
            continue
        for m in c.methods:
            if not code_of(m):
                continue
            evs = jar.events(cname, m)
            frame = Frame(cname, m)
            found = []
            for ev in evs:
                if ev[0] == "binop" and ev[1] in (IAND, IOR):
                    for x, y in ((ev[2], ev[3]), (ev[3], ev[2])):
                        if is_enum_value(x, ecls, acc):
                            for r in resolve_caps_int(jar, y, frame):
                                found.append((enum_const_of(x), r))
                elif ev[0] in ("invoke", "indy"):
                    callee, args, recv = callee_of(jar, ev)
                    if callee is None:
                        continue
                    if ev[0] == "invoke" and ev[2] == ecls and \
                            isinstance(recv, tuple) and recv[0] == "sfield" \
                            and recv[1] == ecls:
                        # E.CONST.matches(caps): the AND is in the enum
                        k = enum_bit_test_arg(jar, ecls, ev[3], ev[4])
                        if k is not None and k < len(args):
                            for r in resolve_caps_int(jar, args[k], frame):
                                found.append((recv[2], r))
                    for i, a in enumerate(args):
                        if not is_enum_value(a, ecls, acc):
                            continue
                        sam = None
                        if ev[0] == "indy":
                            cont = applied_to(evs, ev)
                            sam = (cont, frame) if cont is not None else None
                        sub = Frame(callee[0], callee[1], parent=frame,
                                    recv=recv, args=args, sam_source=sam)
                        for r in follow_mask(jar, sub, sub.local_of_arg(i)):
                            found.append((enum_const_of(a), r))
            for const, (field, default, recv_v, rframe) in found:
                cands = path_of(jar, recv_v, rframe, doc_class) or [None]
                for got in cands:
                    if got is None:
                        path, root, scope = field, None, "unknown"
                    else:
                        path, root = join_path(got[0], field), got[1]
                        scope = (scope_of(got[0]) if root == device_class
                                 else "unknown")
                    h = hits.setdefault((path, scope),
                                        {"root": root, "consts": set(),
                                         "default": default, "field": field})
                    h["consts"].add(const)
    # a field resolved to a path beats the same field left unresolved, and
    # the device document beats another class reaching the same path
    resolved = {v["field"] for (p, s), v in hits.items() if s != "unknown"}
    hits = {k: v for k, v in hits.items()
            if k[1] != "unknown" or v["field"] not in resolved}
    on_device = {p for (p, s) in hits if s != "unknown"}
    hits = {k: v for k, v in hits.items()
            if not (k[1] == "unknown" and k[0] in on_device
                    and v["root"] != device_class)}
    return hits


def enum_scan(jar, doc_class, device_class):
    """Enum bitmaps, each with the document field that consumes it."""
    out = []
    for b in find_bitmask_enums(jar):
        hits = attribute_enum(jar, b, doc_class, device_class)
        rec = dict(b)
        rec["paths"] = [{"path": p, "scope": s, "root_class": v["root"],
                         "default": v["default"],
                         "constants_seen": sorted(x for x in v["consts"] if x)}
                        for (p, s), v in sorted(hits.items())]
        out.append(rec)
    return out


# ======================================================================
# pass 3: bitmaps whose bits are only ever literals
# ======================================================================
#
# fw_caps and friends declare nothing. What they have is a fixed shape: a
# read of getInt("<something>caps") whose value reaches an `iand`. Find
# those reads, find the accessors that mask them, then find every call
# site of those accessors and attribute the literal to the ENCLOSING
# method name -- which is the only place the bit is ever named.

DOC_GET = {"getX", "getList", "getObject", "getOptionalX", "findOne"}


def is_caps_read(v):
    """(field, receiver, default) if v is getInt("<caps>"), else None."""
    v = unspill(v)
    if not v or v[0] != "call":
        return None
    ref, args = v[1], v[2]
    if ref is None or ref[1] not in INT_GETTERS or not args:
        return None
    # value_at keeps the receiver in args[0] for instance calls.
    if args[0][0] == "str" and CAPS_FIELD.match(args[0][1]):
        return args[0][1], None, args[1] if len(args) > 1 else None
    if len(args) > 1 and args[1][0] == "str" and CAPS_FIELD.match(args[1][1]):
        return args[1][1], args[0], args[2] if len(args) > 2 else None
    return None


def doc_field(cf, ins):
    """(field, receiver) if the method just fetches a named child doc."""
    for i, (pc, op, arg) in enumerate(ins):
        if op not in INVOKE or op == 0xba:
            continue
        ref = cf.ref(arg)
        if ref is None or ref[1] not in DOC_GET:
            continue
        recv, args = invoke_operands(cf, ins, i)
        for a in args:
            if a[0] == "str":
                return a[1], recv
    return None, None


def enum_name_table(cf, ins):
    """field name -> (declared NAME, bit value) from an enum <clinit>."""
    out, pending, ints = {}, None, []
    for pc, op, arg in ins:
        if op in (0x12, 0x13):
            k = cf.const(arg)
            if k[0] == "str":
                pending, ints = k[1], []
            elif k[0] == "int" and pending is not None:
                ints.append(k[1])
        elif pending is not None and 0x02 <= op <= 0x08:
            ints.append(op - 0x03)
        elif pending is not None and op in (0x10, 0x11):
            ints.append(arg)
        elif op == PUTSTATIC and pending is not None:
            ref = cf.ref(arg)
            if ref and ref[0] == cf.name and ref[2] == "L%s;" % cf.name:
                out[ref[1]] = (pending, ints[1] if len(ints) > 1 else None)
            pending, ints = None, []
    return out


def shape_pass_one(jar, names):
    """Sub-document getters, whole-bitmap getters and enum name tables."""
    docs, getters, enums = {}, {}, {}
    for cf, m, ins in jar.methods_of(names):
        mref = (cf.name, m["name"], m["desc"])
        reads = [v for v in (
            is_caps_read(value_at(cf, ins, i)[0])
            for i, (pc, op, arg) in enumerate(ins) if op in INVOKE) if v]
        if reads:
            # An int-returning method with a caps read and no mask of its
            # own is the bitmap's getter: hasFirmwareCapability masks
            # *its* result, which is why fw_caps has no accessor of its
            # own to find.
            if m["desc"].endswith(")I") and len(reads) == 1 and \
                    IAND not in {x[1] for x in ins}:
                getters[mref] = reads[0]
        else:
            f, recv = doc_field(cf, ins)
            if f:
                docs[mref] = (f, recv)
        if m["name"] == "<clinit>":
            t = enum_name_table(cf, ins)
            if t:
                enums[cf.name] = t
    return docs, getters, enums


def body_lists(cf, ins, docs):
    """List-typed document fields this method body reaches for.

    Used to place a bitmap whose row the accessor was handed rather than
    fetched, when the expression tree is too tangled to walk.
    """
    out = set()
    for _pc, op, arg in ins:
        if op in (0x12, 0x13):
            k = cf.const(arg)
            if k[0] == "str" and LIST_FIELD.match(k[1]):
                out.add(k[1])
        elif op in INVOKE and op != 0xba:
            got = docs.get(cf.ref(arg))
            if got and LIST_FIELD.match(got[0]):
                out.add(got[0])
    return sorted(out)


def shape_pass_two(jar, names, getters, enums, docs):
    """Accessors: every `iand` fed by a capability read.

    Also records which parameter carries the bit and whether it is an int
    or an enum -- an enum-typed bit parameter means that enum IS the
    bitmap's table.
    """
    out = {}
    for cf, m, ins in jar.methods_of(names):
        static = bool(m["access"] & ACC_STATIC)
        slot_types = dict(param_slots(m["desc"], static))
        mref = (cf.name, m["name"], m["desc"])
        for k, (pc, op, arg) in enumerate(ins):
            if op != IAND:
                continue
            v2, j = value_at(cf, ins, k - 1)
            v1, _ = value_at(cf, ins, j)
            for caps, bit in ((v1, v2), (v2, v1)):
                info = is_caps_read(caps)
                via, u = "read", unspill(caps)
                if info is None and u[0] == "call" and u[1] in getters:
                    f, _r, d = getters[u[1]]
                    info = (f, u[2][0] if u[2] else None, d)
                    via = "getter:" + u[1][1]
                if info is None:
                    continue
                field, recv, dflt = info
                rec = out.setdefault(mref, {
                    "field": field, "field_param": None, "recv": recv,
                    "default": dflt, "static": static, "via": via,
                    "class": cf.name, "bit_params": set(), "inline": [],
                    "doc": None, "_desc": m["desc"], "public": is_public(m),
                    # A list read in the accessor's own body says which
                    # row the document came from when the expression tree
                    # is too tangled to walk.
                    "body_lists": body_lists(cf, ins, docs)})
                if field is None:                 # hasCapability(String,int)
                    rec["field_param"] = u[2][1][2] if u[2] else None
                bit = unspill(bit)
                if bit[0] == "local" and bit[1] == "i" \
                        and bit[2] in slot_types:
                    rec["bit_params"].add(("int", bit[2]))
                elif (bit[0] == "call" and bit[1] and bit[1][2] == "()I"
                      and bit[2] and bit[1][0] in enums  # not Integer.intValue
                      and unspill(bit[2][0])[0] == "local"
                      and unspill(bit[2][0])[2] in slot_types):
                    rec["bit_params"].add(("enum:" + bit[1][0],
                                           unspill(bit[2][0])[2]))
                else:
                    rec["inline"].append(bit)
                break
    return out


def generic_accessors(jar, names):
    """Accessors whose *field* comes from a String parameter.

    hasCapability(String field, int bit) can gate any bitmap at all, so
    its call sites are their own discovery channel. Walked even when it
    finds nothing so the channel is visibly checked rather than assumed
    dead.
    """
    out = {}
    for cf, m, ins in jar.methods_of(names):
        static = bool(m["access"] & ACC_STATIC)
        slot_types = dict(param_slots(m["desc"], static))
        for k, (pc, op, arg) in enumerate(ins):
            if op != IAND:
                continue
            v2, j = value_at(cf, ins, k - 1)
            v1, _ = value_at(cf, ins, j)
            for caps, bit in ((v1, v2), (v2, v1)):
                caps, bit = unspill(caps), unspill(bit)
                if caps[0] != "call" or not caps[1] or \
                        caps[1][1] not in INT_GETTERS:
                    continue
                sargs = [unspill(a) for a in caps[2]]
                strp = [a for a in sargs
                        if a[0] == "local" and a[1] == "a"
                        and slot_types.get(a[2]) == "Ljava/lang/String;"]
                if not strp or any(a[0] == "str" for a in sargs):
                    continue
                if bit[0] == "local" and bit[1] == "i" \
                        and bit[2] in slot_types:
                    out[(cf.name, m["name"], m["desc"])] = {
                        "field_slot": strp[0][2], "bit_slot": bit[2],
                        "static": static}
    return out


def const_value_of(int_fields, owner_cls, v, enums):
    """(value, name, provenance) for a bit expression, or None."""
    v = unspill(v)
    if not v:
        return None
    if v[0] == "int":
        return v[1], None, "literal"
    if v[0] == "sfield":
        if not v[1]:
            return None
        owner, fname, ftype = v[1]
        if ftype == "L%s;" % owner and owner in enums:
            got = enums[owner].get(fname)
            if got and got[1] is not None:
                return got[1], got[0], "enum-constant"
        if ftype == "I" and owner == owner_cls and fname in int_fields:
            return int_fields[fname], fname, "named-constant"
        return None
    if v[0] == "call" and v[1] and v[1][2] == "()I" and v[2]:
        inner = unspill(v[2][0])
        if inner and inner[0] == "sfield":
            return const_value_of(int_fields, owner_cls, inner, enums)
    return None


def enum_source(v):
    """Enum class an int argument was computed from, if any.

    hasSpeedCapability(idx, SpeedDuplex.of(s, d).value()) never names a
    bit at the call site -- but it does say which enum enumerates the
    bitmap, which is the pointer worth keeping.
    """
    v = unspill(v)
    if not v:
        return None
    if v[0] == "call":
        if v[1] and v[1][2] == "()I" and v[1][0] not in ("java/lang/Integer",):
            return v[1][0]
        for a in v[2]:
            got = enum_source(a)
            if got:
                return got
    if v[0] == "sfield" and v[1] and v[1][2] == "L%s;" % v[1][0]:
        return v[1][0]
    return None


def shape_pass_three(jar, names, accessors, enums, docs, generics):
    """Call sites of every accessor, with the bit each one passes.

    The enclosing method name is the payload: supportsMacBasedVlans
    calling hasFirmwareCapability(-2147483648) is the only place the
    controller ever names fw_caps bit 31.
    """
    sites, forwarders = {}, {}
    byname = defaultdict(list)
    for mref in sorted(accessors):
        byname[(mref[1], mref[2])].append((mref, accessors[mref]))
    for mref in sorted(generics):
        byname[(mref[1], mref[2])].append((mref, None))
    wanted = {n.encode() for n, _d in byname}
    hot = [n for n in names if any(w in jar.blobs[n] for w in wanted)]
    for cf, m, ins in jar.methods_of(hot):
        static = bool(m["access"] & ACC_STATIC)
        slot_types = dict(param_slots(m["desc"], static))
        caller = (cf.name, m["name"], m["desc"])
        for i, (pc, op, arg) in enumerate(ins):
            if op not in INVOKE:
                continue
            ref, kind, captured = cf.ref(arg), None, False
            if op == 0xba:
                impl = cf.indy_impl(arg)
                if not impl:
                    continue
                ref, kind = impl
                captured = True
            if ref is None:
                continue
            hit = [x for x in byname.get((ref[1], ref[2]), [])
                   if x[0][0] == ref[0]]
            if not hit or hit[0][0] == caller:
                continue
            mref, a = hit[0]
            recv, args = invoke_operands(cf, ins, i)
            if captured:
                # Captured arguments bind to the lambda body's leading
                # parameters; an instance body takes `this` first.
                args = ([recv] if kind in (5, 7, 9) and recv != UNKNOWN
                        else []) + args
            if a is None:                       # hasCapability(String,int)
                g = generics[mref]
                order = [s for s, _t in param_slots(mref[2], g["static"])]
                if len(args) < len(order):
                    continue
                f = unspill(args[order.index(g["field_slot"])])
                b = unspill(args[order.index(g["bit_slot"])])
                if f[0] == "str" and CAPS_FIELD.match(f[1]) and b[0] == "int":
                    sites[(f[1], b[1], caller, mref)] = {
                        "field": f[1], "value": b[1], "accessor": mref,
                        "caller": caller, "const_name": None,
                        "provenance": "literal", "doc": None,
                        "enum_source": None}
                continue
            order = [s for s, _t in param_slots(mref[2], a["static"])]
            doc = doc_evidence(args, order, a, docs)
            for kind_, slot in sorted(a["bit_params"]):
                if slot not in order:
                    continue
                pos = order.index(slot)
                if pos >= len(args):
                    continue
                val = unspill(args[pos])
                if val[0] == "local" and val[2] in slot_types:
                    # The caller passes its own parameter through: it is
                    # another accessor for the same bitmap, so register it
                    # and run the pass again.
                    f = forwarders.setdefault(caller, dict(
                        a, via="forwards:%s" % mref[1], class_=cf.name,
                        bit_params=set(), inline=[],
                        doc=doc or a.get("doc"), _desc=m["desc"],
                        static=static, public=is_public(m),
                        body_lists=body_lists(cf, ins, docs)
                        or a.get("body_lists")))
                    f["bit_params"].add((kind_, val[2]))
                    continue
                got = const_value_of(cf.int_fields, cf.name, args[pos], enums)
                if got is None:
                    src = enum_source(args[pos])
                    if src:
                        sites[("enum-src", a["field"], src, caller)] = {
                            "field": a["field"], "value": None,
                            "accessor": mref, "caller": caller,
                            "const_name": None, "provenance": None,
                            "doc": doc, "enum_source": src}
                    continue
                value, cname, prov = got
                sites[(a["field"], value, caller, mref)] = {
                    "field": a["field"], "value": value, "accessor": mref,
                    "caller": caller, "const_name": cname, "provenance": prov,
                    "doc": doc, "enum_source": enum_source(args[pos])}
    return list(sites.values()), forwarders


def doc_evidence(args, order, a, docs):
    """Which document this call site handed the accessor."""
    recv = unspill(a["recv"]) if a["recv"] else None
    if not recv or recv[0] != "local" or recv[1] != "a" or recv[2] == 0:
        return None
    if recv[2] not in order:
        return None
    pos = order.index(recv[2])
    return container_of(args[pos], docs) if pos < len(args) else None


def container_of(v, docs, depth=0):
    """Nearest named document field an expression was fetched from."""
    v = unspill(v)
    if not v or depth > 4:
        return None
    if v[0] == "call":
        ref, args = v[1], v[2]
        if ref in docs:
            return docs[ref][0]
        if ref and ref[1] in DOC_GET:
            for a in args:
                if a[0] == "str":
                    return a[1]
        for a in args:
            got = container_of(a, docs, depth + 1)
            if got:
                return got
    return None


def resolve_shape_path(a, docs, device_cls):
    """(document path, scope, why) for one accessor."""
    recv = unspill(a["recv"]) if a["recv"] else None
    field = a["field"]
    parent = container_of(recv, docs) if recv and recv[0] == "call" else None
    if not parent:
        parent = a.get("doc")
    if not parent and len(a.get("body_lists") or []) == 1:
        parent = a["body_lists"][0]
    if not parent and recv and recv[0] == "local" and recv[1] == "a":
        if recv[2] == 0 and not a["static"]:
            owner = a.get("class_") or a["class"]
            return (field, "device" if owner == device_cls else "unknown",
                    "the accessor reads it off its own document")
        t = dict(param_slots(a["_desc"], a["static"])).get(recv[2])
        if t == "L%s;" % device_cls:
            return (field, "device",
                    "the accessor is handed the device document")
        return (field, "unknown",
                "the accessor is handed a document it did not fetch")
    if parent:
        m = LIST_FIELD.match(parent)
        if m:
            return ("%s[].%s" % (parent, field),
                    CONTAINER_SCOPE.get(m.group(1), m.group(1)),
                    "fetched from the list %s" % parent)
        return ("%s.%s" % (parent, field), "device",
                "fetched from the sub-document %s" % parent)
    return field, "unknown", "the receiver could not be resolved"


def cooccurrence(jar, names, fields):
    """How often each caps field shares a method with a *_table field.

    Only ever a *hint* for a bitmap whose container the bytecode never
    names: the controller reads radio_caps off a row it was handed, so
    the row's origin is not in the accessor at all.
    """
    out = defaultdict(Counter)
    for cf, m, ins in jar.methods_of(names):
        strs = {cf.const(a)[1] for _p, o, a in ins
                if o in (0x12, 0x13) and cf.const(a)[0] == "str"}
        hit = strs & fields
        if not hit:
            continue
        for lst in sorted(s for s in strs if LIST_FIELD.match(s)):
            for f in sorted(hit):
                out[f][lst] += 1
    return out


def scope_hint(field, counts):
    """(scope, evidence) guess for a bitmap with no resolved container."""
    word = field.split("_", 1)[0]
    if word in CONTAINER_SCOPE:
        return CONTAINER_SCOPE[word], "the field name starts with %r" % word
    top = counts.most_common(2) if counts else []
    if top and top[0][1] >= 2 and (len(top) == 1 or top[0][1] > top[1][1]):
        m = LIST_FIELD.match(top[0][0])
        if m and m.group(1) in CONTAINER_SCOPE:
            return (CONTAINER_SCOPE[m.group(1)],
                    "it shares a method with %s %d times"
                    % (top[0][0], top[0][1]))
    return None, None


def document_reader(jar, ref, doc_class):
    """Is this a call to a public method of the document wrapper?

    The obfuscator is not as tidy as one would like: it renamed the
    document's own private helpers, but it also renamed public methods of
    internal service classes, so "public" alone is not a stable name.
    What is stable is the document wrapper's public API -- getX, getInt,
    put, append are spelled the same in both images -- and that is also
    the only part worth reporting here, because the question is which
    document read touched the field.
    """
    if ref[0] is None or not is_typed_doc(jar, ref[0], doc_class):
        return False
    cur, hops = ref[0], 0
    while cur and hops < 4:
        c = jar.cls(cur)
        if c is None:
            return False
        for m in c.methods:
            if m["name"] == ref[1] and m["desc"] == ref[2]:
                return is_public(m) and cur == doc_class
        cur, hops = c.supername, hops + 1
    return False


def unclaimed_fields(jar, names, found, doc_class):
    """Caps-named document fields with no mask site to prove them.

    A bitmap read into a builder and masked off that object later never
    shows getInt and iand together, so it cannot be confirmed here.
    Listing the leftovers -- with the document methods each is read
    through -- keeps them visible instead of silently absent, which is
    the failure mode this whole file exists to fix.
    """
    out = defaultdict(lambda: defaultdict(int))
    for cf, m, ins in jar.methods_of(names):
        for e in cf.cp:                         # every string, so a field
            if not e or e[0] != CONSTANT_Utf8:  # only ever used in a table
                continue                        # still shows up
            if CAPS_FIELD.match(e[1]) and e[1] not in found:
                out[e[1]]
        for i, (pc, op, arg) in enumerate(ins):
            if op not in INVOKE or op == 0xba:
                continue
            ref = cf.ref(arg)
            if ref is None or not document_reader(jar, ref, doc_class):
                continue
            _recv, args = invoke_operands(cf, ins, i)
            for a in args:
                a = unspill(a)
                if a[0] == "str" and CAPS_FIELD.match(a[1]) \
                        and a[1] not in found:
                    out[a[1]][ref[1]] += 1
    return {f: dict(sorted(v.items(), key=lambda kv: (-kv[1], kv[0])))
            for f, v in sorted(out.items())}


def shape_scan(jar, names, doc_class, device_class, max_rounds=4):
    """Bitmaps discovered from the shape of a masked document read."""
    docs, getters, enums = shape_pass_one(jar, names)
    accessors = shape_pass_two(jar, names, getters, enums, docs)
    generics = generic_accessors(jar, names)
    sites, known, rounds = [], dict(accessors), 0
    while rounds < max_rounds:
        got, fwd = shape_pass_three(jar, names, known, enums, docs, generics)
        sites = got
        fresh = {k: v for k, v in fwd.items() if k not in known}
        if not fresh:
            break
        known.update(fresh)
        rounds += 1
        generics = {}          # only worth walking once
    fields = {a["field"] for a in known.values() if a["field"]}
    return {"accessors": known, "sites": sites, "docs": docs, "enums": enums,
            "generics": generics, "device_class": device_class,
            "cooccurrence": cooccurrence(jar, names, fields),
            "unclaimed": unclaimed_fields(jar, names, fields, doc_class)}


# ======================================================================
# who is the device document
# ======================================================================


def find_document_class(jar, names):
    """The JSON-document wrapper: the class owning getInt(String)I.

    Found by shape rather than by name, because the name is obfuscated
    differently in every build.
    """
    counts = Counter()
    for name in names:
        c = jar.cls(name)
        if not c:
            continue
        for i, cpe in enumerate(c.cp):
            if cpe and cpe[0] == CONSTANT_Methodref:
                r = c.ref(i)
                if r and r[1] == "getInt" and r[2] == "(Ljava/lang/String;)I":
                    counts[r[0]] += 1
    if not counts:
        return None
    return sorted(counts.items(), key=lambda kv: (-kv[1], kv[0]))[0][0]


def find_device_class(jar, names, doc_class):
    """The device model: the typed document reading the most *_caps keys."""
    best, best_n = None, 0
    for name in sorted(names):
        c = jar.cls(name)
        if not c or c.supername != doc_class:
            continue
        keys = {cpe[1] for cpe in c.cp if cpe and cpe[0] == CONSTANT_Utf8
                and cpe[1].endswith("_caps")}
        if len(keys) > best_n:
            best, best_n = name, len(keys)
    return best


# ======================================================================
# merging what the three passes found
# ======================================================================


def snake(name):
    """supportsMacBasedVlans -> MAC_BASED_VLANS (None if not a wrapper)."""
    m = WRAPPER.match(name)
    if not m or not m.group(1):
        return None
    s = STRIP_TAIL.sub("", m.group(1)) or m.group(1)
    for a, b in ACRONYM.items():
        s = s.replace(a, b)
    s = re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", s)
    s = re.sub(r"([A-Z]+)([A-Z][a-z])", r"\1_\2", s)
    return s.upper()


class Bitmap(object):
    """One capability bitmap, however its bits came to be known."""

    def __init__(self, field):
        self.field = field
        self.places = []                      # (path, scope, why)
        self.accessors = Counter()
        self.defaults = set()
        self.candidates = defaultdict(list)   # value -> [(provenance, name)]
        self.masks = {}                       # name -> value
        self.family = None
        self.notes = []

    def add(self, value, name, provenance):
        if value == 0:
            return
        if not is_bit(value):
            if name:
                self.masks[name] = value
            return
        if (provenance, name) not in self.candidates[value]:
            self.candidates[value].append((provenance, name))

    def place(self, path, scope, why):
        if (path, scope, why) not in self.places:
            self.places.append((path, scope, why))

    def best(self):
        """The most specific placement, so path, scope and evidence agree.

        Several accessors can reach the same bitmap and resolve it to
        different depths. Reporting the deepest one that also pinned a
        scope keeps the three fields telling one story instead of three.
        """
        if not self.places:
            return (None, "unknown", None)
        return sorted(self.places,
                      key=lambda p: (p[1] == "unknown", -p[0].count("."),
                                     -len(p[0]), p[0]))[0]

    def accessor(self):
        """The name callers use, or None if only unprintable ones read it.

        Only names that passed both stability guards were ever counted
        (see merge), so this just picks the most-used of them.
        """
        if not self.accessors:
            return None
        return sorted(self.accessors.items(),
                      key=lambda kv: (-kv[1], kv[0]))[0][0]


def rank_of(provenance):
    return PROVENANCE_RANK.index(provenance) \
        if provenance in PROVENANCE_RANK else len(PROVENANCE_RANK)


def resolve_names(bitmap):
    """Pick each bit's name by provenance, keeping the runners-up.

    A declared constant beats an enum constant beats the UI's own table
    beats a name paraphrased from a wrapper method. A bit with no name at
    all keeps its value: inventing one would be worse than admitting it.
    """
    bits, prov, alts, unnamed = {}, {}, {}, []
    taken = set()
    for value in sorted(bitmap.candidates, key=unsigned):
        ranked = sorted((c for c in bitmap.candidates[value] if c[1]),
                        key=lambda c: (rank_of(c[0]), c[1]))
        chosen = next(((p, n) for p, n in ranked if n not in taken), None)
        if chosen is None:
            unnamed.append(value)
            continue
        taken.add(chosen[1])
        bits[chosen[1]] = value
        prov[chosen[1]] = chosen[0]
        others = [{"name": n, "provenance": p} for p, n in ranked
                  if n != chosen[1]]
        if others:
            alts[chosen[1]] = others
    return bits, prov, alts, unnamed


def family_field(family, accessor_field, maps):
    """Document field a constant family belongs to.

    Three joins, strongest first: the hand-recorded one for the switch
    families, which the jar genuinely does not spell out; the accessor the
    family is named after; and the family's own name, because UNIFI_UDAPI_
    CAP and udapi_caps are the same word. No join means the family is
    still emitted, with a null field.
    """
    named = FAMILY_ACCESSOR.get(family)
    if named and named in accessor_field:
        return accessor_field[named]
    stem = re.sub(r"_CAP2?$", "", stem_of(family))
    want = tokens_of(stem)
    if want:
        hits = sorted({f for a, f in accessor_field.items()
                       if want <= tokens_of(a)})
        if len(hits) == 1:
            return hits[0]
    for suffix in ("_caps", "_caps2", ""):
        target = stem.lower() + suffix
        if target in maps:
            return target
    return None


def loose_field(prefix, maps):
    """Bitmap a loose constant family belongs to, or None.

    Placed only on the unique bitmap whose field or accessor name contains
    the constants' own stem: UNIFI_USG2_* on usg2_caps, UNIFI_ERROR_* on
    the bitmap hasSystemError reads. Ambiguity means no placement -- the
    constants stay visible in the unplaced section instead.
    """
    stem = tokens_of(stem_of(prefix).split("_")[0])
    if not stem or stem <= {"DEFAULT", "INITIAL", "UNIFI"}:
        return None
    hits = []
    for field in sorted(maps):
        words = tokens_of(field)
        for accessor in maps[field].accessors:
            if WRAPPER.match(accessor):
                words |= tokens_of(accessor)
        if stem <= words:
            hits.append(field)
    return hits[0] if len(hits) == 1 else None


def merge(consts, loose, shape, enums, device_class, ui_tables):
    """Fold the three passes into one table, keyed by document field."""
    maps = {}

    def bucket(field):
        if field not in maps:
            maps[field] = Bitmap(field)
        return maps[field]

    # --- shape scan: paths, scopes, accessors, method-named bits --------
    for mref in sorted(shape["accessors"]):
        a = shape["accessors"][mref]
        if not a["field"]:
            continue
        b = bucket(a["field"])
        b.place(*resolve_shape_path(a, shape["docs"], device_class))
        if a.get("public") and WRAPPER.match(mref[1]):
            b.accessors[mref[1]] += 1
        d = unspill(a["default"]) if a["default"] else None
        if d and d[0] == "int":
            b.defaults.add(d[1])
        for v in a["inline"]:
            got = const_value_of({}, None, v, shape["enums"])
            if got and got[1]:
                b.add(got[0], got[1], got[2])
    enum_tables = defaultdict(set)
    for s in sorted(shape["sites"],
                    key=lambda s: (s["field"] or "", unsigned(s["value"] or 0),
                                   s["caller"])):
        if not s["field"]:
            continue
        b = bucket(s["field"])
        if s["enum_source"] and shape["enums"].get(s["enum_source"]):
            enum_tables[s["field"]].add(s["enum_source"])
        if s["value"] is None:
            continue
        name, prov = s["const_name"], s["provenance"]
        if prov not in ("named-constant", "enum-constant"):
            name, prov = snake(s["caller"][1]), "method-name"
        b.add(s["value"], name, prov if name else "literal")
    # An enum-typed bit parameter (or an argument computed from one) means
    # that enum IS the bitmap's table: take every constant, but grade it
    # as "this enum enumerates this bitmap", not "this bit was seen used".
    for field in sorted(enum_tables):
        for cls in sorted(enum_tables[field]):
            for fname in sorted(shape["enums"].get(cls, {})):
                nm, val = shape["enums"][cls][fname]
                if val:
                    bucket(field).add(val, nm, "enum-table")

    # --- enum bitmaps: proven bit tables with a document path -----------
    unattributed = []
    for e in enums:
        placed = [p for p in e["paths"] if p["scope"] != "unknown"]
        if not placed:
            table = {x["name"]: x["value"] for x in e["bits"]}
            # An enum whose values are consecutive is a plain code enum
            # (DISABLED/OPTIONAL/REQUIRED, a protobuf oneof case) that
            # only looks like a bitmap because 1 and 2 are both powers of
            # two. Nothing masks it against anything, so say so once here
            # rather than list two dozen of them.
            values = sorted(table.values())
            if values == list(range(values[0], values[0] + len(values))):
                continue
            if table not in unattributed:
                unattributed.append(table)
            continue
        for p in placed:
            field = p["path"].split(".")[-1]
            b = bucket(field)
            b.place(p["path"], p["scope"],
                    "an enum bitmap is masked against it")
            if p["default"] is not None:
                b.defaults.add(p["default"])
            for x in e["bits"]:
                if x["value"]:
                    b.add(x["value"], x["name"], "enum-constant")

    # --- declared constants: the strongest names ------------------------
    accessor_field = {}
    for mref in sorted(shape["accessors"]):
        a = shape["accessors"][mref]
        if a["field"] and a.get("public") and WRAPPER.match(mref[1]):
            accessor_field.setdefault(mref[1], a["field"])
    unplaced_families = {}
    for family in sorted(consts):
        field = family_field(family, accessor_field, maps)
        if field is None:
            unplaced_families[family] = consts[family]
            continue
        b = bucket(field)
        b.family = family
        for name in sorted(consts[family]):
            b.add(consts[family][name], name, "named-constant")

    # --- loose constants: bits whose name names no family ---------------
    other = {}
    groups = loose_groups(loose)
    for prefix in sorted(groups):
        members = groups[prefix]
        field = loose_field(prefix, maps)
        if field is None:
            other.update(members)
            continue
        b = bucket(field)
        if len(members) > 1 and b.family is None:
            b.family = prefix
        for name in sorted(members):
            b.add(members[name], name, "named-constant")
        b.notes.append(
            "%s placed here by name -- it matches this bitmap and no other, "
            "but no call site passes these constants" % prefix)

    # --- the controller UI's own tables ---------------------------------
    # The UI names bits the controller never tests (fw_caps SSH, say), so
    # these are added rather than only consulted; bit_provenance says
    # where each came from.
    for field in sorted(ui_tables):
        if field not in maps:
            continue
        for value in sorted(ui_tables[field]):
            maps[field].add(value, ui_tables[field][value], "ui-bundle")

    # --- scope hints for the ones the bytecode never places -------------
    for field in sorted(maps):
        b = maps[field]
        path, scope, why = b.best()
        if scope == "unknown":
            hint, hwhy = scope_hint(field, shape["cooccurrence"].get(field))
            if hint:
                b.place(path or field, hint, why)
                b.notes.append("scope is a guess, not a proof: %s" % hwhy)

    return maps, other, unplaced_families, unattributed


# ======================================================================
# rendering
# ======================================================================


def render(maps, other, unplaced_families, unattributed, shape):
    """The committed table: only build-stable facts, sorted."""
    bitmaps = {}
    placed_fields = set()
    for field in sorted(maps):
        b = maps[field]
        placed_fields.add(b.field)
        path, scope, why = b.best()
        bits, prov, alts, unnamed = resolve_names(b)
        order = sorted(bits, key=lambda k: unsigned(bits[k]))
        entry = {
            "accessor": b.accessor(),
            "field": b.field,
            "path": path,
            "scope": scope,
            "evidence": why,
            "bits": {k: bits[k] for k in order},
            "bit_provenance": {k: prov[k] for k in order},
        }
        if len(b.defaults) == 1:
            entry["default"] = sorted(b.defaults)[0]
        elif b.defaults:
            entry["defaults"] = sorted(b.defaults)
        if b.masks:
            entry["masks"] = dict(sorted(b.masks.items(),
                                         key=lambda kv: kv[1]))
        if unnamed:
            entry["unnamed_bits"] = sorted(unnamed, key=unsigned)
        if alts:
            entry["alternate_names"] = {k: alts[k] for k in sorted(alts)}
        if b.notes:
            entry["notes"] = sorted(b.notes)
        bitmaps[b.family or path or b.field] = entry
    for family in sorted(unplaced_families):
        bits = unplaced_families[family]
        order = sorted(bits, key=lambda k: unsigned(bits[k]))
        bitmaps[family] = {
            "accessor": None, "field": None, "path": None, "scope": "unknown",
            "evidence": "no accessor in this build reads a document field "
                        "with these constants",
            "bits": {k: bits[k] for k in order},
            "bit_provenance": {k: "named-constant" for k in order},
        }
    unplaced = {
        "_note": (
            "what the scan turned up and could not place, kept visible "
            "because a silent drop is how the enum bitmaps stayed missing. "
            "constants: capability-namespace ints that belong to no "
            "bitmap. bitmap_enums: enums whose constants are distinct "
            "powers of two but which nothing masks against a document "
            "field, minus the ones whose values are merely consecutive "
            "(a plain code enum looks like a bitmap when its codes are 1 "
            "and 2). fields: caps-named document fields with no proven "
            "mask site, and the document methods they are read through; a "
            "bitmap read into a builder and masked off that object later "
            "never shows getInt and iand together."
        ),
        "constants": dict(sorted(other.items())),
        "bitmap_enums": sorted(unattributed, key=lambda u: sorted(u)),
        "fields": {f: v for f, v in sorted(shape["unclaimed"].items())
                   if f not in placed_fields},
    }
    return dict(sorted(bitmaps.items())), unplaced


def verify_ui_tables(swai_path, tables):
    """Confirm every embedded UI name still carries its value in the bundle.

    Both ends need a boundary or the check passes on the drift it exists to
    catch: a plain substring test for "SSH:1" is satisfied by SSH:16, and
    "DPI:64" is satisfied by UDPI:64.
    """
    text = open(swai_path, "r", errors="replace").read()
    out = {}
    for field, table in tables.items():
        out[field] = {
            n: re.search(r"(?<![A-Za-z0-9_$])%s:%d(?![0-9])"
                         % (re.escape(n), v), text) is not None
            for v, n in table.items()}
    return out


# ======================================================================
# one image
# ======================================================================


def extract(path, ui_tables):
    """(bitmaps, unplaced, per-image provenance) for one jar."""
    jar = Jar(path)
    families, loose, records, empty = scan_constants(jar)
    if not records:
        sys.exit("no capability constants in %s" % path)

    # Scope the analysis to the archive member that ships the constants: a
    # fat jar carries Spring and Bouncy Castle beside the application, and
    # comparing two packagings only means something if both runs read the
    # same code.
    container = Counter(r["container"] for r in records).most_common(1)[0][0]
    jar.select(container)

    caps_classes = sorted(n for n, d in jar.blobs.items() if b"caps" in d)
    doc_class = find_document_class(jar, caps_classes)
    device_class = find_device_class(jar, caps_classes, doc_class)
    names = sorted(jar.blobs)

    shape = shape_scan(jar, names, doc_class, device_class)
    enums = enum_scan(jar, doc_class, device_class)
    maps, other, unplaced_families, unattributed = merge(
        families, loose, shape, enums, device_class, ui_tables)
    bitmaps, unplaced = render(maps, other, unplaced_families, unattributed,
                               shape)

    classes = []
    for r in records:
        rec = {"class": r["class"], "constants": r["constants"]}
        if r["container"]:
            rec["nested_in"] = r["container"]
        classes.append(rec)
    source = {
        "classes": classes,
        # Obfuscated per build, so these belong to the image and never to
        # the shared table. Recorded because they are what the analysis
        # hung everything else off.
        "document_class": doc_class,
        "device_model_class": device_class,
        "classes_analysed": len(names),
    }
    if empty:
        flagged = []
        for e in empty:
            rec = {"class": e["class"], "why": e["why"]}
            if e["container"]:
                rec["nested_in"] = e["container"]
            flagged.append(rec)
        source["flagged_without_constants"] = flagged
    return bitmaps, unplaced, source


def main():
    ap = argparse.ArgumentParser()
    # LOCAL is the copied-out jar to read; PATH is where it lives in the
    # image, which is what belongs in the committed file -- a host temp
    # path would change on every run.
    ap.add_argument("--source", nargs=4,
                    metavar=("LOCAL", "IMAGE", "VERSION", "PATH"),
                    action="append", required=True)
    ap.add_argument("--swai", help="controller UI bundle, to re-verify the "
                                   "embedded UI bit tables before using them")
    ap.add_argument("--out", default="-")
    args = ap.parse_args()

    if args.swai:
        stale = {}
        for field, checked in verify_ui_tables(args.swai, UI_TABLES).items():
            gone = sorted(n for n, ok in checked.items() if not ok)
            if gone:
                stale[field] = gone
        if stale:
            sys.exit("UI bit tables are stale for %s: re-derive them from "
                     "the bundle before trusting them" % stale)

    bitmaps, unplaced, seen = None, None, []
    for local, image, version, path in args.source:
        got_bitmaps, got_unplaced, source = extract(local, UI_TABLES)
        if not seen:
            bitmaps, unplaced = got_bitmaps, got_unplaced
        elif got_bitmaps != bitmaps or got_unplaced != unplaced:
            sys.exit("%s disagrees with %s on the capability table; "
                     "extract them separately and diff"
                     % (image, seen[0]["image"]))
        record = {"image": image, "controller_version": version, "jar": path}
        record.update(source)
        seen.append(record)

    versions = sorted({s["controller_version"] for s in seen})
    doc = {
        "_source_note": (
            "generated by scripts/extract-caps.sh from the Network "
            "application jar; bit names exist nowhere else. Values are "
            "Java ints, so a bit-31 constant is negative. Keyed by "
            "constant family where the jar declares one, else by document "
            "path -- a bitmap whose bits are only ever literals has no "
            "family name to key on. bit_provenance grades every name: "
            "named-constant and enum-constant are the vendor's own "
            "identifiers, ui-bundle is the controller UI's name for that "
            "bit, enum-table means the enum is this bitmap's table but "
            "that bit was not seen used, and method-name is paraphrased "
            "from the wrapper method that passes the literal and is the "
            "weakest of the five."
        ),
        "controller_version": versions[0] if len(versions) == 1 else versions,
        "sources": seen,
        "bitmaps": bitmaps,
        # Everything the scan turned up and could not place. Recorded so
        # the extraction is visibly complete rather than quietly filtered.
        "unplaced": unplaced,
    }
    text = json.dumps(doc, indent=2) + "\n"
    if args.out == "-":
        sys.stdout.write(text)
    else:
        Path(args.out).write_text(text)
        total = sum(len(b["bits"]) for b in bitmaps.values())
        print("wrote %s: %d bitmaps, %d bits"
              % (args.out, len(bitmaps), total))


if __name__ == "__main__":
    main()
