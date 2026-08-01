# runtime-fixture

A generic OCI runtime image the herder can start like any other device
runtime. It emulates nothing — it exists so the herder's container contract
is provable in public, with a test double anyone can build from this tree.

## What it proves

A qualifying runtime image is handed two environment variables and left
alone. This fixture is the executable statement of that:

| Contract | How the fixture shows it |
| --- | --- |
| Two variables in, nothing else | Reads `UNIFI_EMU_INFORM_URL` and `UNIFI_EMU_DEVICES_JSON`; logs a warning naming any other `UNIFI_EMU_*` variable it was passed |
| One device per container | `UNIFI_EMU_DEVICES_JSON` is a JSON array; zero, two or more is fatal |
| No silent drift | Strict decoding — only `model`, `mac`, `serial`, `name`; an unknown key is fatal |
| Bad input never runs | Missing variable, malformed JSON or an unusable inform URL logs to stderr and exits non-zero, so the container never reports healthy |
| Identities come from the herder | The supplied model/MAC/serial/name and inform URL are used verbatim; none are defaulted or invented |
| The container knows its own address | Reports the Docker-assigned endpoint IPv4, found by walking the interfaces for the first non-loopback IPv4 (169.254/16 skipped — that is the address a container with no network ends up with) |
| It informs | POSTs `{model, mac, serial, name, ip, inform_url}` to the inform URL every 2s |
| Health means alive, not adopted | A failed POST is logged and the loop continues: an absent or late controller must not take the fleet down |
| Health is self-reported | The process writes a readiness marker holding its pid once its input validates; `HEALTHCHECK` runs the same binary with `-healthcheck`, which is the only way to express this in an image with no shell |
| It stops when told | SIGTERM/SIGINT abort any in-flight POST and exit 0 in well under a second |
| It needs nothing from the host | No bind mount, no command, no credentials, no Docker socket, no `EXPOSE` — it only dials out |

## Building

The build context is the repository root, because the module's `go.mod`
lives there. The package imports the standard library only.

```
docker build -f internal/herder/testdata/runtime-fixture/Dockerfile -t <local-tag> .
```

The tag is the caller's business; the herder's tests build and remove it.

The image is `linux/amd64`: the binary is always cross-compiled for amd64,
and the final stage asks for that platform. A `scratch` base carries no
platform of its own, so a builder without BuildKit ignores the request and
stamps the image with the host's architecture instead — the contents are
unaffected, but `docker image inspect` will say `arm64` on an arm64 host.

## Running it by hand

```
docker run --rm \
  -e UNIFI_EMU_INFORM_URL=http://172.18.0.1:8080/inform \
  -e UNIFI_EMU_DEVICES_JSON='[{"model":"U7PRO","mac":"02:00:00:00:00:01","serial":"EMU000000001","name":"fixture-1"}]' \
  <local-tag>
```

`go test ./internal/herder/testdata/runtime-fixture` covers the decisions —
decoding, address selection, payload shape, the retry-forever loop and the
probe — without needing Docker at all.
