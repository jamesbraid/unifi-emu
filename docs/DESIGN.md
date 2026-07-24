# unifi-emu — design

## What it is

A Go library + CLI/daemon that emulates UniFi devices (UAP / USW / UGW) speaking
the real UniFi **inform** protocol, so a real UniFi controller discovers, adopts,
and manages them as if they were hardware. It exists to give integration test
harnesses — `go-unifi` and `terraform-provider-unifi` — real, deterministic,
controllable devices, especially on a UniFi OS controller whose built-in demo
devices are absent.

**Hard rule:** devices enter a controller **only through the real inform/adoption
lifecycle**. No MongoDB/DB seeding — DB-injected devices render permanently
disconnected because connection state is derived from a live inform heartbeat.

## When you actually need it (scope discipline)

Not every test target needs the emulator. Decide per target:

| Target | Devices already present? | Device source |
|---|---|---|
| Classic Network App `-sim` (e.g. `ghcr.io/jamesbraid/unifi-network:sim`) | Yes — demo devices, deterministic MACs, pending/adoptable | **Adopt a demo device** — no emu |
| UOS `-sim` (`ghcr.io/jamesbraid/unifi-os-server:sim`) | Yes — demo devices on the Network-App API | Adopt a demo device — no emu |
| **UOS `-seeded`** (`ghcr.io/jamesbraid/unifi-os-server:seeded`, UOS-native `:443`) | **No** | **unifi-emu** |
| Any target needing specific models / counts / port layouts / connected payloads | Partially | **unifi-emu** for control |

Where demo devices exist, the cheap move is "adopt a demo device by MAC". The
emulator is for the seeded/UOS-native path and for controllable fleets. It should
reuse the *same* adopt helper — it differs only in *who supplies the pending
device* (the controller's demo seeder vs. this emulator).

## Verified protocol facts (from a working spike)

- Inform endpoint: HTTP **POST** to `http://<controller>:8080/inform`.
- Packet header: magic `TNBU`, then version, device MAC, flags, IV,
  payload-version, payload-length, payload. Flag bits: `0x01` encrypted, `0x02`
  zlib, `0x04` snappy, `0x08` AES-GCM. Devices begin with AES-128-CBC and switch
  later informs to AES-GCM when `mgmt_cfg.use_aes_gcm=true`. Compression is zlib
  or snappy.
- Default adoption key (unadopted devices): `ba86f2bbe107c7c57eb5f2690775c712`.
- **Adoption handshake:**
  1. Device informs with the DEFAULT key: payload `state=1, default=true,
     adopted=0`, every ~5–10 s. Controller lists it as pending (`stat/device`
     `state=2`).
  2. An admin issues `adopt` (controller-side API call).
  3. The controller delivers a NEW authkey to the device's *next* inform —
     **two channels exist**: the documented `{_type:"cmd", cmd:"set-adopt",
     key, uri}`, or `mgmt_cfg.authkey` inside a `setparam`. Verified live
     (2026-07-22, `unifi-network:10.4.57-sim`): that build **never sends
     set-adopt** — mgmt_cfg.authkey is the only channel, and it equals the
     device doc's `x_authkey`. The emu supports both; the mgmt_cfg key is
     adopted only while the device is still on the default key.
  4. Device switches to the new key + uri, keeps informing → controller sends
     `setparam`/`mgmt_cfg` → device becomes managed (`state=1, adopted=true`).
- **Two things the reference implementations get wrong — do not repeat:**
  - `mgmt_cfg.authkey` must not be saved *unconditionally* — that is the
    classic stuck-loop bug (OpenUniFi ADOPTION_FIX.md). Gate it on still
    holding the default key; never let a later mgmt_cfg clobber a real key.
  - The device must inform **continuously** through the whole handshake. A
    one-shot "inform once then idle" design never completes adoption (times
    out to `state=7`, adopt-failed).
- **HTTP 404 on unadopted informs is benign** (resolved, was Phase A's open
  question): it means "nothing queued for this device". Verified: a pending
    device logs 404s until the moment adoption is queued, then flips to 200
    carrying the adoption mgmt_cfg — same packet format throughout.
- **The inform URL host must be an IP literal.** The controller validates the
  device-reported `inform_url` and rejects hostnames post-adoption
  (`invalid inform_ip unifi` → HTTP 400). The integration harness puts the
  controller and emulator on one isolated Testcontainers network and passes the
  controller's container IPv4 address. The CLI also resolves DNS names to IPv4
  once at startup for other container-network consumers.
- Device MAC must be a fixed, caller-supplied value carried in the payload — NOT
  derived from a container interface (docker reassigns container MACs on restart).
- **One gateway per site:** adopting a 2nd UGW fails with `api.err.NoSecondGateway`.
  Fleets use APs/switches; at most one gateway.
- `stat/device` states: `1`=connected, `2`=pending, `7`=adopt-failed.

## Architecture

### Core engine (library)

Per simulated device, a goroutine running the inform state machine:

```go
type DeviceSpec struct {
    MAC     string // fixed, caller-chosen (e.g. 00:27:22:00:00:02)
    Type    string // "uap" | "usw" | "ugw"
    Model   string // "US8P60", "U7PIW", "UGW3", ...
    Version string // firmware, e.g. 4.4.36.5146617
    Name    string // optional
    // type-specific shape: ports (usw), radios/vaps (uap), wan (ugw)
}

func New(informURL string, opts ...Option) *Emu   // informURL = http://controller:8080/inform
func (e *Emu) Add(specs ...DeviceSpec) error
func (e *Emu) Start(ctx context.Context) error     // continuous inform loops
func (e *Emu) State(mac string) (DeviceState, bool) // PENDING | ADOPTING | CONNECTED
func (e *Emu) WaitState(ctx context.Context, mac string, want DeviceState) error
func (e *Emu) Stop()
```

State machine per device: **PENDING** (inform with DEFAULT key) → on `set-adopt`,
switch key+uri → **ADOPTING** (inform with new key, apply `setparam`/`mgmt_cfg`) →
**CONNECTED**. Handle `noop` (interval), `reboot`/`upgrade`/`setdefault`.

Crypto/wire (AES-128-CBC + optional GCM, zlib/snappy, TNBU header): port from the
references below; unit-test round-trip encode/decode before touching a controller.

### Payload shaping — use a `-sim` controller as the oracle

The only real reverse-engineering risk is the per-device-type inform payload
(gateway `sys_stats`/WAN vs switch `port_table` vs AP `radio_table`/`vap_table`).
Don't guess: boot `ghcr.io/jamesbraid/unifi-network:sim`, log in `admin`/`admin`,
`GET /api/s/default/stat/device`, and template the payload fields from those real,
version-matched device documents. (Hand-author minimal payload structs — don't
commit Ubiquiti-derived JSON verbatim.)

### Adopt helper (both auth flavors)

A helper that turns a *pending* device into an *adopted, connected* one, so a test
gets a one-call "give me an adopted device":

- **Classic** (`:8443`): cookie session via `POST /api/login`, then
  `POST /api/s/<site>/cmd/devmgr {cmd:"adopt",mac}`.
- **UOS-native** (`:443`): `POST /api/auth/login` → session cookie +
  `x-updated-csrf-token`, then `POST /proxy/network/api/s/<site>/cmd/devmgr`
  with the `X-CSRF-Token` header.

(`go-unifi` already abstracts both login styles; the helper can reuse it.)

## Integration

### Repository live tests

The `integration`-tagged suite owns every resource through
`testcontainers-go`. Each test creates a network, fresh controller, and
checkout-built emulator container. The host reaches the controller API through
a random mapped port. Device informs stay on the shared network.

`TestClassicUGWLive` proves one gateway, `TestClassicFleetLive` serially adopts
the five-device fleet, and `TestUOSAPUpgradeLive` proves the seeded-UOS
login/CSRF/adoption/AES-GCM/firmware-upgrade cycle. The UOS request reproduces
the host cgroup namespace, cgroup bind, capabilities, and tmpfs contract used by
`run-uos.sh`. Every test writes full logs and pending/final device documents
under `tmp/itest/<test-name>/` before removing its containers and network.

### go-unifi (`internal/controllertest`)

The harness exists and defaults to `ghcr.io/jamesbraid/unifi-network:*-sim`
(classic, `:8443`, `admin`/`admin`), exposing a raw `Session`
(`GetJSON`/`PostJSON`/…). Integrate in two small PRs:

1. A controller-agnostic `Controller.AdoptDevice(ctx, t, session, mac)` built on
   the existing `Session` — adopts a *demo* device today (no emu), and is the seam
   the emulator plugs into. Independently useful; land it first.
2. `StartDeviceSim(ctx, t, controller, specs...)` — starts the emu library against
   the controller's inform endpoint, feeds `AdoptDevice`. Requires exposing
   `8080/tcp` on the harness container and a `UNIFI_TEST_URL` no-op guard (never
   inform/mutate a real controller). Integration-gated (`//go:build integration`).

### terraform-provider-unifi (compose)

The provider brings up a compose sim controller (port 8080 already exposed) and
its device test adopts a demo device (`mac = 00:27:22:00:00:02`, `allow_adoption`).
Add an `unifi-device-sim` service to `docker-compose.yaml` on the same network
(`SIM_CONTROLLER=http://unifi:8080/inform`, `SIM_DEVICES=[…]`); the provider's own
`allow_adoption` does the adopt. Needed only when the harness swaps to the seeded
UOS image (which has no demo devices).

## Build phasing — DONE (2026-07-22)

All phases landed; see the README status section. Historical record:

- **A — engine to CONNECTED (gateway).** Live-proven; the 404 question is
  resolved (benign "nothing queued" — see the protocol facts above).
- **B — switch + AP payloads.** Live-proven fleet: 1 UGW + 2 USW + 2 UAP, all
  `state=1 adopted=true` (host-mode and in-container).
- **C — library + adopt helpers + container image + CLI.** Shipped; the UOS
  helper and negotiated AES-GCM inform path are live-proven against the
  published seeded image.
- **D — go-unifi PR** (`AdoptDevice` + `StartDeviceSim` + inform port):
  jamesbraid/go-unifi#16.
- **E — provider PR** (compose sidecar, profile-gated):
  jamesbraid/terraform-provider-unifi#11.

Remaining: publish the module + image (both PRs note it).

## References

- Protocol/crypto reference (Python, MIT): <https://github.com/amd989/unifi-gateway>
  (`unifi_protocol.py` — correct wire format; gateway-only, one-shot flow — the
  bug to avoid).
- AP-side + adoption state machine (C): <https://github.com/dachsbaerli/OpenUniFi>
  (read `ADOPTION_FIX.md` in full, plus `src/crypto.c`, `src/inform.c`).
- Protocol write-up: <https://jrjparks.github.io/unofficial-unifi-guide/protocols/inform.html>
- Consumers: <https://github.com/ubiquiti-community/go-unifi>,
  <https://github.com/ubiquiti-community/terraform-provider-unifi>
- Controller test images: `ghcr.io/jamesbraid/unifi-os-server` (`:sim`, `:seeded`),
  `ghcr.io/jamesbraid/unifi-network` (`:sim`).
