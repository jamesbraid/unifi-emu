# unifi-emu

<p align="center">
  <img src="assets/logo.png" alt="unifi-emu — an emu holding a UniFi AP and a switch" width="280">
</p>

**Fake UniFi devices that speak the inform protocol and get adopted by a real
controller.** A device simulator/emulator for integration testing — give a UniFi
controller a fleet of deterministic, controllable APs / switches / gateways
without any hardware.

`emu` = emulator (and a flightless bird that struts around pretending it belongs).

## Status

🐓 **Fully fledged.** A live-proven fleet — 1 gateway, 2 switches, 2 APs —
adopts all the way to CONNECTED against a real controller
(`ghcr.io/jamesbraid/unifi-network:sim`), including a controller-requested
firmware "upgrade" survived with an emulated reboot. Shipped:

- **Library** (`package emu`) — fleet API: `New/Add/Start/State/WaitState/Stop`.
- **CLI** (`cmd/unifi-emu`) — single-device flags, a full `-devices` file /
  `SIM_DEVICES` env (YAML/JSON), or a terse `-models` / `SIM_MODELS` list
  (`U7PRO,USM8P:2,UGW3`) that auto-derives MAC/IP.
- **Container image** — `docker build -t unifi-emu:dev .` (static, scratch,
  ~9MB). Bakes the 5-device fleet as its default, so a bare `docker run`
  boots it; `-e SIM_MODELS=…`, `-e SIM_DEVICES=…`, or the flags override it.
  The live suite builds this image from the checkout.
- **Adopt helpers** — classic Network App (`ClassicClient`) and UniFi OS
  ucore/CSRF (`UOSClient`), live-proven against the published seeded UOS
  image through its controller-requested AP firmware upgrade.
- **Consumer integrations** — `AdoptDevice` + `StartDeviceSim` in go-unifi's
  controllertest (jamesbraid/go-unifi#16) and a compose sidecar in
  terraform-provider-unifi (jamesbraid/terraform-provider-unifi#11).

Not yet: the module/image aren't published anywhere (both PRs note it).

### Quick start

```sh
go test ./... # unit tests, no container runtime needed
go test -tags integration -run TestClassicUGWLive -v -count=1 .
go test -tags integration -run TestClassicFleetLive -v -count=1 .
go test -tags integration -run TestUOSAPUpgradeLive -v -count=1 .
docker build -t unifi-emu:dev . && docker run --rm unifi-emu:dev -h
```

The image boots its baked default fleet, and any explicit source overrides it:

```sh
docker run --rm unifi-emu:dev -inform http://CTRL:8080/inform                 # baked 5-device fleet
docker run --rm -e SIM_MODELS=U7PRO,USM8P:2,UGW3 unifi-emu:dev -inform …       # pick models (MAC/IP auto)
docker run --rm -e SIM_DEVICES="$(cat my-fleet.yaml)" unifi-emu:dev -inform …  # full control
```

`SIM_MODELS` counts models (`USM8P:2` = two of them) and derives each
device's MAC/IP from `SIM_MAC_BASE`/`SIM_IP_BASE` (defaults
`00:27:22:e0:00:00` / `192.168.1.100`); a fleet may hold at most one gateway.

The live tests use `testcontainers-go`. Each test creates an isolated network,
a fresh controller, and an emulator container built from the checkout.
Controller APIs use random host ports. Inform traffic stays on the container
network. Logs and device documents remain under `tmp/itest/<test-name>/`.

Set `UNIFI_EMU_ITEST_EMULATOR_IMAGE` to test a prebuilt emulator instead.
`UNIFI_EMU_ITEST_CLASSIC_IMAGE` and `UNIFI_EMU_ITEST_UOS_IMAGE` select
controller images. Defaults are `ghcr.io/jamesbraid/unifi-network:sim` and
`ghcr.io/jamesbraid/unifi-os-server:seeded`.

The newer UOS path uses a fresh seeded controller and proves the negotiated
CBC-to-AES-GCM transition and AP firmware upgrade. Its controller healthcheck
stays enabled. The harness also waits for seeded-owner and API readiness.

### Model registry

The registry covers the full current UniFi AP / switch / gateway lineup —
**182 models** at controller 10.4.57. [`model_profiles.json`](model_profiles.json)
is the checked-in reduced catalog; it is embedded (`go:embed`) and parsed at
startup, so there is no generated Go to commit and `go build`/`go test` need no
extra step.

`cmd/modelgen` builds the catalog from a controller's hardware database plus a
couple of Ubiquiti sources. The bundle isn't in git, so refreshing needs a
controller — a deliberate step, not part of the build. Harvest from a controller
the UI's `swai.*.js` bundle (every model's ports and radios) and
`.../dl/firmware/bundles.json` (display names), and fetch Ubiquiti's
`firmware-latest` and device `fingerprint` JSON. Then:

```sh
# real AP ethernet from Tech Specs, written into model_overrides.json
go run ./cmd/modelgen -fetch-eth -bundle swai.js -fingerprint fingerprint.json

# generate the catalog
go run ./cmd/modelgen -bundle swai.js -bundles-json bundles.json \
  -firmware-json firmware-latest.json -overrides model_overrides.json \
  -controller-version 10.4.57
go test ./...
```

Firmware versions come from Ubiquiti's fw-update API, joined on the model code.
AP Ethernet — which the hardware DB omits — comes from Tech Specs, matched to the
fingerprint DB by model code and then by hardware sysid (so the hex-coded 10GbE
flagships resolve). Facts the bundle can't express live in
[`model_overrides.json`](model_overrides.json). A model the bundle can't render —
an unknown radio band, say — is skipped and logged, never emitted wrong; an AP
with no resolved ethernet keeps a 1×GbE default, also logged.

## More

- [`docs/DESIGN.md`](docs/DESIGN.md) — what it is, the verified inform-protocol
  facts, architecture, and how it plugs into `go-unifi` / `terraform-provider-unifi`.
- [`docs/BUILD-PROMPT.md`](docs/BUILD-PROMPT.md) — the kickoff plan for the first
  build phase (a gateway that adopts to CONNECTED).

## The one hard rule

Devices enter a controller **only through the real inform/adoption lifecycle** —
no MongoDB/DB seeding. DB-injected devices render permanently disconnected; the
whole point of this tool is real, connected, adoptable devices.

## License

MIT — see [LICENSE](LICENSE).
