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
- **CLI** (`cmd/unifi-emu`) — single-device flags, `-devices` file, or
  `SIM_DEVICES` env (YAML/JSON).
- **Container image** — `docker build -t unifi-emu:dev .` (static, scratch,
  ~9MB). In-container adoption proven on a pinned docker network.
- **Adopt helpers** — classic Network App (`ClassicClient`) and UniFi OS
  ucore/CSRF (`UOSClient`), live-proven against the published seeded UOS
  image through its controller-requested AP firmware upgrade.
- **Consumer integrations** — `AdoptDevice` + `StartDeviceSim` in go-unifi's
  controllertest (jamesbraid/go-unifi#16) and a compose sidecar in
  terraform-provider-unifi (jamesbraid/terraform-provider-unifi#11).

Not yet: the module/image aren't published anywhere (both PRs note it).

### Quick start

```sh
go test ./...            # unit tests, no controller needed
bash scripts/itest.sh    # live proof: one gateway adopts to CONNECTED (docker)
bash scripts/itest.sh fleet   # live proof: the whole 5-device fleet
bash scripts/itest.sh docker  # live proof: sim runs inside a container
docker build -t unifi-emu:dev . && docker run --rm unifi-emu:dev -h
```

The Go-level live tests sit behind the `integration` build tag and two env
vars (one live test per fresh controller — recreate between runs):

```sh
UNIFI_EMU_TEST_INFORM_URL=http://127.0.0.1:8080/inform \
UNIFI_EMU_TEST_API_URL=https://localhost:8443 \
go test -tags integration -run TestEmuAdoptsFleetLive -v .
```

The newer UOS path uses a fresh seeded controller and proves the negotiated
CBC-to-AES-GCM transition as well as the AP firmware upgrade:

```sh
run-uos.sh uos-seeded-reverse ghcr.io/jamesbraid/unifi-os-server:seeded \
  --no-healthcheck -p 12443:443 -p 19080:8080

UNIFI_EMU_TEST_UOS_INFORM_URL=http://127.0.0.1:19080/inform \
UNIFI_EMU_TEST_UOS_API_URL=https://localhost:12443 \
go test -tags integration -run TestEmuAdoptsUOSLive -v .
```

### Model registry

| Model | Type | Firmware |
|---|---|---|
| UGW3 | gateway | 4.4.36.5146617 |
| USWED74 | switch | 4.0.21.9965 |
| USM8P | switch (PoE) | 4.0.21.9965 |
| US48P750 | switch (PoE) | 4.0.21.9965 |
| USWED06 | switch | 4.0.21.9965 |
| USWF07D | switch | 4.0.21.9965 |
| U7MP | access point | 4.0.21.9965 |
| U7PRO | access point | 4.0.21.9965 |
| UAPA6B0 | access point | 4.0.21.9965 |

The registry is generated, not hand-shaped. [`model_profiles.json`](model_profiles.json)
is the checked-in reduced fixture and `go generate ./...` renders
`models_generated.go` from it. The fixture records the source controller
version and keeps the complete expanded port and radio layouts so review diffs
show every hardware change.

To refresh it from a controller build, save:

- `GET /api/s/default/stat/device` for model IDs, names, types, and firmware;
- the controller UI's `swai.*.js` bundle, which contains its hardware database.

Then run:

```sh
go run ./cmd/modelgen \
  -input stat-device.json \
  -device-db-bundle swai.js \
  -controller-version 10.4.57
go test ./...
```

The reducer rejects missing models, duplicate IDs or ports, type mismatches,
unknown port encodings, empty layouts, and incomplete AP radio data. The
controller also exposes `GET /v2/api/site/default/models`; that endpoint is
useful for identity/image metadata but does not include port or radio layouts.
The few facts absent from both dumps (AP Ethernet speed/count and radio spatial
streams) follow Ubiquiti's Tech Specs for
[AC Mesh Pro](https://techspecs.ui.com/unifi/wifi/uap-ac-mesh-pro),
[U7 Pro](https://techspecs.ui.com/unifi/wifi/u7-pro),
[U7 Pro Outdoor](https://techspecs.ui.com/unifi/wifi/u7-pro-outdoor-us), and
[Ultra](https://techspecs.ui.com/unifi/switching/usw-ultra).

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
