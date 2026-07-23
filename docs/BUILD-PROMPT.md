# Build kickoff prompt — Phase A

> **Historical (2026-07-22): the build is complete** — this prompt did its
> job. Kept for the record; [`DESIGN.md`](DESIGN.md) tracks as-built reality.

Paste this to a fresh build agent working in this repo. Read
[`DESIGN.md`](DESIGN.md) first — it's the source of truth; this prompt is the
Phase-A marching orders.

---

```
You're building unifi-emu: fake UniFi devices that speak the "inform" protocol and
get adopted by a real UniFi controller, for integration testing. Read docs/DESIGN.md
first — it has the verified protocol facts, architecture, and integration plan.

Phase A (this task): a simulated UniFi gateway (UGW) that adopts into a real
controller via the inform protocol and reaches CONNECTED state. Get ONE device type
fully adopted, then STOP and report. Don't build the switch/AP payloads or the
consumer integrations yet.

HARD CONSTRAINT: devices enter only through the real inform/adoption lifecycle.
No DB seeding.

Steps:
1. Port the crypto/wire format (TNBU header, AES-128-CBC, zlib/snappy, default key
   ba86f2bbe107c7c57eb5f2690775c712) from amd989/unifi-gateway's unifi_protocol.py
   to Go. TDD it: encode→decode round-trip and packet-parse tests that need no
   controller. See github.com/amd989/unifi-gateway and github.com/dachsbaerli/OpenUniFi
   (ADOPTION_FIX.md is essential for the key-rotation state machine).
2. Implement one UGW device with a CONTINUOUS inform loop (every ~5-10s) and the
   full adoption state machine: DEFAULT key → controller set-adopt (new key + uri)
   → switch key → mgmt_cfg key. Fixed, caller-supplied MAC in the payload.
3. Boot a controller and drive the device to stat/device state=1, adopted=true:
   - Simplest: ghcr.io/jamesbraid/unifi-network:sim (classic, :8443, admin/admin,
     cookie auth via /api/login, adopt via POST /api/s/default/cmd/devmgr
     {cmd:"adopt",mac}).
   - Real target: ghcr.io/jamesbraid/unifi-os-server:seeded (UOS, owner admin/admin
     on :443; adopt via /api/auth/login -> session + x-updated-csrf-token ->
     POST /proxy/network/api/s/default/cmd/devmgr {cmd:"adopt",mac} with
     X-CSRF-Token). Runtime contract for the UOS image: cgroupns=host + the cap
     list + tmpfs + expose 8080 — see the unifi-os examples in the unifi-containers
     repo.
   Topology that works: a pinned docker bridge network, controller at a static IP
   with that IP advertised as the inform host (UOS_SYSTEM_IP), port 8080 exposed;
   the emu on the same network informing http://<controller-ip>:8080/inform. Pin the
   emu's MAC (docker randomizes container MACs on restart, which breaks adoption).
   NOTE: colima can't bind-mount /private/tmp — put mounted config under ~/.cache.
4. Resolve the spike's open question: unadopted informs returned HTTP 404. Determine
   whether that's "nothing queued until adopt" (expected) or a response-parse
   mismatch — a continuously-informing device plus a mid-stream adopt settles it.
   READ the controller's own response bodies and /usr/lib/unifi/logs/server.log
   rather than theorizing.

Payload shaping: don't guess the gateway inform payload. Boot the -sim image, log in
admin/admin, GET /api/s/default/stat/device, and template your payload fields from
that real version-matched UGW document. Hand-author minimal structs; don't commit
Ubiquiti-derived JSON verbatim.

Constraints: one gateway per site (api.err.NoSecondGateway) — fine for Phase A
(single UGW). Device states: 1=connected, 2=pending, 7=adopt-failed.

Norms: TDD the protocol layer. Kernel-style commit messages (subsystem: imperative
summary, why-first body). Integration tests that need docker go behind a build tag
(//go:build integration) so `go test ./...` stays docker-free. Read failing
components' own logs before theorizing.

Stop after Phase A and report: what adopted, the exact payload that worked, and
whether the 404 was benign. Phases B (switch/AP payloads), C (library + image), and
D/E (consumer PRs) are specced in DESIGN.md and come next.
```
