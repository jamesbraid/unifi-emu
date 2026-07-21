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

🐣 **Bootstrapping.** Nothing built yet. Start here:

- [`docs/DESIGN.md`](docs/DESIGN.md) — what it is, the verified inform-protocol
  facts, architecture, and how it plugs into `go-unifi` / `terraform-provider-unifi`.
- [`docs/BUILD-PROMPT.md`](docs/BUILD-PROMPT.md) — a ready-to-run kickoff prompt
  for the first build phase (a gateway that adopts to CONNECTED).

## The one hard rule

Devices enter a controller **only through the real inform/adoption lifecycle** —
no MongoDB/DB seeding. DB-injected devices render permanently disconnected; the
whole point of this tool is real, connected, adoptable devices.

## License

MIT — see [LICENSE](LICENSE).
