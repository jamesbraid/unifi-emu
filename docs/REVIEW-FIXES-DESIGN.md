# PR 1 review fixes

## Scope

Fix the behavioral findings from the PR 1 review while preserving the
emulator's test-tool assumptions:

- keep all nine simulated models
- keep full `mgmt_cfg` logging, including authkeys
- keep the adopt helpers' insecure TLS transport for disposable test
  controllers
- avoid runtime dependencies on a source controller or the public internet

## Model profiles

The controller's simulation mode is the source of truth. A fresh
`unifi-network:10.4.57-sim` controller seeds one gateway, five switches, and
three access points. Adopt those nine devices, wait for each to reach
`state=1 adopted=true`, then capture `stat/device`.

A refresh tool reduces that response to the facts the emulator needs:

- model identifier, type, display name, and firmware
- ordered ports with index, interface name, media, PoE capability, and uplink
  status
- ordered radios with band, channel width, power range, spatial streams, and
  radio capabilities

The repository stores only this reduced fixture, not the raw controller
documents. Generated Go profiles come from the fixture. Tests require exactly
the nine supported model identifiers and compare every generated port and
radio field with the fixture. They also reject duplicate port indexes,
duplicate interface names, impossible counts, and model/type mismatches.

The public product specifications are a cross-check for product names and
physical port counts. Controller metadata wins when the simulator's internal
model differs from a current retail product.

## Runtime fixes

- Decode UniFi's JSON envelope on successful HTTP responses. Return
  `meta.msg` when `meta.rc` is not `ok`. Apply the same rule to login, adopt,
  and device queries.
- Treat `cmd=reboot` as an emulated reboot by resetting uptime while preserving
  the adoption key, configuration, firmware, and connection state.
- Accept only six-byte Ethernet MAC addresses before adding a device.
- Reject non-positive fleet inform intervals from `Start`.
- Use either CSRF response header accepted by the UOS transport during login.
- Return a startup error when a hostname cannot resolve to IPv4. Do not start a
  fleet with an `inform_url` the controller is known to reject.
- Stop reporting a global `required_version`. The controller oracle does not
  emit it, and one value cannot describe all nine models.

## Verification

Each behavior starts with a focused failing test. The model refresh path is
checked against a captured reduced fixture, and the generated registry is
checked for a clean diff. Final verification runs:

```text
go test ./... -race -count=1
go vet ./...
go test ./... -tags integration -run '^$'
git diff --check
```

The live controller proof adopts all nine built-in simulation devices and then
runs the emulator's existing fleet and container modes.
