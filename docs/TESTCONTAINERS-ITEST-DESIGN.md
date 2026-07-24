# Testcontainers integration harness

## Goal

Replace `scripts/itest.sh` and the externally managed live-test setup with
self-contained Go integration tests. The harness must start real controllers
and the emulator through `testcontainers-go`, exercise the inform/adoption
lifecycle, preserve useful evidence, and clean up every resource it creates.

The replacement has no Bash compatibility wrapper and never invokes the Docker
CLI.

## Test matrix

The integration suite covers:

- one classic-controller device through `state=1, adopted=true`
- the classic five-device gateway/switch/AP fleet
- the emulator image built from the current checkout and run as a container
- one seeded-UOS U7 Pro through login, CSRF, adoption, AES-GCM negotiation,
  firmware upgrade, and final controller `state=1, adopted=true`

All tests use the repository's existing `integration` build tag.

## Topology

Each test creates an isolated Testcontainers network. The controller and
emulator containers join that network, so inform traffic stays
container-to-container. The controller API is exposed on a random mapped host
port for the Go test process.

This removes the fixed `8443`, `8080`, `11443`, and `18080` host ports. It also
removes the classic harness's `SYSTEM_IP=127.0.0.1` workaround: a controller
without that override advertises its container address, which the emulator can
reach on the shared network.

The emulator image is built from the current checkout by default. Environment
variables may select prebuilt emulator or controller images, including an
unpublished seeded-UOS candidate.

## Container definitions

The classic controller uses a normal Testcontainers `ContainerRequest` with
the shared network, exposed API port, and a health wait strategy.

The UOS request reproduces the runtime contract currently expressed by
`run-uos.sh`:

- host cgroup namespace
- `/sys/fs/cgroup` mounted read/write
- all capabilities dropped, then only the documented capabilities added
- executable tmpfs mounts for `/run`, `/run/lock`, and `/tmp`
- tmpfs mounts for the journal and UniFi temporary data

The new seeded image's healthcheck remains enabled. Readiness requires both the
container health signal and seeded-owner/API readiness. The harness does not
implement login-rate-limit retries.

## Harness structure

Container request construction, readiness polling, evidence capture, and
cleanup live in integration-tagged Go helpers. Tests use the existing `Emu`,
`ClassicClient`, and `UOSClient` APIs for behavior assertions.

Testcontainers cleanup hooks terminate emulator and controller containers and
remove the network even after a failed assertion. Helpers return ordinary
errors with the current phase and relevant container state.

## Evidence

Each test writes to `tmp/itest/<test-name>/`:

- controller container logs
- Network application `server.log`
- emulator container logs
- pending and final device documents

Failure paths capture evidence before cleanup. Successful runs retain their
final documents and logs so the same artifacts support local debugging and CI
receipts.

## Verification

Unit tests inspect the generated container requests and image-selection rules
without requiring a container runtime. The integration-tag compile gate keeps
the complete harness buildable in ordinary CI.

Live verification runs the classic single/fleet cases and the seeded-UOS
upgrade case against fresh Testcontainers-managed resources. The UOS case must
finish on the controller-requested firmware with controller state 1 and
adopted true.
