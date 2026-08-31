# unifi-emu

<p align="center">
  <img src="assets/logo.png" alt="unifi-emu — an emu holding a UniFi AP and a switch" width="280">
</p>

Fake UniFi devices that speak the real inform protocol and get adopted by a real
controller. It gives an integration test a deterministic fleet of APs, switches,
and gateways — controllable, reproducible, and without any hardware.

## Why it exists

Anything that drives a UniFi controller — [go-unifi][go-unifi], the
[Terraform provider][tf], your own tooling — needs devices on that controller
that are genuinely connected and adoptable. Real hardware is slow and
non-deterministic. Seeding the controller's database doesn't work either: a
device injected that way shows up permanently disconnected, because the
controller derives connection state from a live inform heartbeat.

The only way in is the real path: a device that informs, gets adopted, and keeps
informing to stay connected. unifi-emu is that device, in software — a whole
fleet of them, deterministic and scriptable.

## What you get

- **A Go library** (`package emu`) — build a fleet and drive it: `New`, `Add`,
  `Start`, `State`, `WaitState`, `Stop`.
- **A CLI** (`unifi-emu`) — run devices from flags, a fleet file, or a terse
  model list like `U7PRO,USM8P:2,UGW3`.
- **A container image** — `docker run` a fleet against a controller, and
  optionally have it adopt the fleet itself, so nothing outside needs an adopt
  client.
- **A herder** (`unifi-emu-herder`) — start a fleet as containers on a Docker
  network you already own, and read back their MACs, serials, and addresses as
  NDJSON on stdout.

It covers the current UniFi AP, switch, and gateway lineup, adopts all the way to
connected against a real controller, and survives a controller-requested
firmware upgrade by faking the reboot.

## Quick start

```sh
go test ./...                                  # unit tests, no runtime needed

docker build -t unifi-emu:dev .
docker run --rm unifi-emu:dev -inform http://CONTROLLER:8080/inform
```

The image bakes a default fleet, so a bare `docker run` boots it. Pick models
with `-e SIM_MODELS=U7PRO,USM8P:2,UGW3` (MAC and IP are derived), or hand it a
full fleet with `-e SIM_DEVICES="$(cat fleet.yaml)"`.

Pull the image or install the herder:

```sh
docker pull ghcr.io/jamesbraid/unifi-emu:latest
go install github.com/jamesbraid/unifi-emu/cmd/unifi-emu-herder@latest
```

The [usage guide](docs/USAGE.md) covers self-adoption, running as a CI service,
and the integration suite.

## The one rule

A device enters a controller **only** through the real inform and adoption
lifecycle — never by seeding the database. A database-injected device renders
permanently disconnected. The whole point is real, connected, adoptable devices.

## Documentation

- [Usage guide](docs/USAGE.md) — CLI flags, self-adoption settings, running as a
  CI service, and the integration suite.
- [Protocol spec](docs/PROTOCOL.md) — the wire-level UniFi device protocol
  (inform and L2 discovery), for reusing or porting it to other firmware.
- [Design](docs/DESIGN.md) — architecture, and how it plugs into go-unifi and the
  Terraform provider.

## License

MIT — see [LICENSE](LICENSE).

[go-unifi]: https://github.com/ubiquiti-community/go-unifi
[tf]: https://github.com/ubiquiti-community/terraform-provider-unifi
