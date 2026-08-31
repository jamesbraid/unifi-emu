# Usage guide

How to configure a fleet, have it adopt itself, run it as a CI service, and run
the integration suite. For what unifi-emu is and why, see the
[README](../README.md).

## Installing

The image is on GHCR — multi-arch (`linux/amd64`, `linux/arm64`), scratch, and
anonymously pullable. The module is on the Go proxy. Image tags drop the `v` that
module tags carry.

```sh
docker pull ghcr.io/jamesbraid/unifi-emu:latest
go get github.com/jamesbraid/unifi-emu@latest
go install github.com/jamesbraid/unifi-emu/cmd/unifi-emu-herder@latest
```

In GitHub Actions, install the herder with the bundled action. It needs no Go
toolchain and checksums the release archive before unpacking it. Pin it to a
release tag:

```yaml
- uses: jamesbraid/unifi-emu/.github/actions/install-herder@v0.5.2
```

The herder runs the image built from its own release tag, so the herder and the
devices it starts come from one commit. A `go install ...@vX.Y.Z` binary reads
that tag from the version the Go command records. A build from a working tree has
no tag and asks for `--synthetic-image`.

## Configuring a fleet

Three ways to say what devices to run, in increasing order of control.

- **A model list** — `SIM_MODELS` (env) or `-models` (flag): a comma-separated
  list, with an optional count per model. `U7PRO,USM8P:2,UGW3` is one U7PRO, two
  USM8P, one UGW3. Each device's MAC and IP derive from `SIM_MAC_BASE` and
  `SIM_IP_BASE` (defaults `00:27:22:e0:00:00` and `192.168.1.100`).
- **A fleet file** — `SIM_DEVICES` (env, holding the file contents) or
  `-devices` (flag, a path): YAML or JSON, one entry per device, with full
  control over MAC, serial, model, IP, name, ports, and SSIDs.
- **Single-device flags** — `-mac`, `-model`, `-ip`, and so on, for one device.

A fleet holds at most one gateway: adopting a second fails with
`api.err.NoSecondGateway`.

`SIM_CONTROLLER` is an alias for the `-inform` flag, so a service block that sets
it needs no command.

## Self-adoption

Without `-adopt`, the devices inform and sit pending, waiting for someone to
click Adopt. With it, the container logs into the controller's API and adopts
them itself, so nothing outside needs an adopt client:

```sh
docker run --rm --network unifi \
  -e SIM_ADOPT=1 \
  -e SIM_ADOPT_URL=https://controller:8443 \
  -e SIM_ADOPT_USERNAME=admin -e SIM_ADOPT_PASSWORD=admin \
  unifi-emu:dev -inform http://controller:8080/inform
```

Every flag reads its default from the matching variable, so an explicit flag
beats the environment, and both beat the built-in default. Credentials have no
flag, because a password in `argv` shows up in every process listing.

| Variable | Flag | Default | Meaning |
|---|---|---|---|
| `SIM_ADOPT` | `-adopt` | off | Adopt the fleet after it informs |
| `SIM_ADOPT_URL` | `-adopt-url` | — | Controller **API** URL (required) |
| `SIM_ADOPT_USERNAME` | — | — | Controller login (required) |
| `SIM_ADOPT_PASSWORD` | — | — | Its password (required) |
| `SIM_ADOPT_PASSWORD_FILE` | — | — | Read the password from this file instead |
| `SIM_ADOPT_SITE` | `-adopt-site` | `default` | Site to adopt into |
| `SIM_ADOPT_DIALECT` | `-adopt-dialect` | from the URL port | `classic` or `unifios` |
| `SIM_ADOPT_TIMEOUT` | `-adopt-timeout` | `5m` | Budget for the whole adoption |

The container refuses to start when a setting is missing or malformed, because a
container that informs but silently never adopts looks exactly like one that is
merely slow. Setting any `SIM_ADOPT_*` variable without `SIM_ADOPT` is an error
for the same reason. To override an environment that turns adoption on, pass
`-adopt=false`.

### Two things that trip people up

**The adopt URL is the API port**, not the inform port: 8443 on the classic
Network App, 443 on UniFi OS. The container therefore needs to reach both — 8080
to inform, and the API port to adopt.

**The dialect is a guess about a port**, not a fact about the controller. Port
443 gives `unifios`, anything else gives `classic`. The container logs which way
it resolved. Set `-adopt-dialect` when the guess is wrong, as it will be for a
controller behind a proxy.

### Adoption behaviour

Devices adopt one at a time. Controllers reject bursts and reject documents that
are only seconds old, so adopting in sequence finishes sooner than adopting in
parallel.

The container tolerates a controller that is still booting. Informing retries
forever, and login retries for as long as `-adopt-timeout` allows. A service that
starts ahead of its controller adopts as soon as that controller answers.
Rejected credentials still fail on the first answer, rather than burning the
budget.

Readiness is the body, not the status. Early in boot a controller serves an HTML
placeholder under HTTP 200 on every path, so the status alone can't spot it.
Every answer from a running controller is JSON, a refusal included, so the wait
is for a JSON body. Pointing `SIM_ADOPT_URL` at the inform port fails
immediately rather than waiting, because that mistake reaches a controller that
answers, and a controller that answers answers in JSON.

Failure is fatal and loud. When a device never reaches connected inside
`-adopt-timeout`, the container reports what the controller thought of every
device, stops the fleet, and exits non-zero. Success is quiet: the container
keeps running and informing until you stop it.

## As a CI service

Compose and Woodpecker start services before the test step, so the emulator
informs and adopts on its own while the pipeline waits for connected devices.

```yaml
services:
  - name: emu
    image: unifi-emu:dev
    environment:
      SIM_CONTROLLER: http://controller:8080/inform
      SIM_MODELS: U7PRO,USM8P:2
      SIM_ADOPT: "1"
      SIM_ADOPT_URL: https://controller:8443
      SIM_ADOPT_SITE: default
      SIM_ADOPT_TIMEOUT: 10m
      SIM_ADOPT_USERNAME: admin
      SIM_ADOPT_PASSWORD:
        from_secret: controller_password
```

Where the runner mounts secrets as files rather than variables, point
`SIM_ADOPT_PASSWORD_FILE` at the path instead. Give `SIM_ADOPT_TIMEOUT` room for
the controller to boot and the fleet to adopt: the default 5m suits a controller
that is already up, and a cold one can spend most of that starting.

## Integration tests

The live suite uses `testcontainers-go`. Each test creates an isolated network, a
fresh controller, and an emulator container built from the checkout. Controller
APIs use random host ports, and inform traffic stays on the container network.

```sh
go test -tags integration -run TestClassicUGWLive        -v -count=1 .
go test -tags integration -run TestClassicFleetLive      -v -count=1 .
go test -tags integration -run TestClassicContainerAdoptLive -v -count=1 .
go test -tags integration -run TestUOSAPUpgradeLive      -v -count=1 .
```

`UNIFI_EMU_ITEST_EMULATOR_IMAGE` tests a prebuilt emulator instead of building
one. `UNIFI_EMU_ITEST_CLASSIC_IMAGE` and `UNIFI_EMU_ITEST_UOS_IMAGE` select the
controller images.

The UniFi OS path boots a fresh seeded controller and proves the negotiated
CBC-to-AES-GCM transition and the AP firmware upgrade. Before starting, the
harness waits for that container's healthcheck, its seeded owner, and its API.
