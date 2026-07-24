# UOS seeded live-adoption — session findings & handoff

Status as of 2026-07-23. Written for an agent picking up the **post-upgrade
provisioning** drill (the one open problem) while the main line moves on.

## TL;DR

The `:443` UniFi-OS-native adoption path **works end-to-end live** against
`ghcr.io/jamesbraid/unifi-os-server:seeded`: login → CSRF → `/proxy/network`
adopt → authkey rotation → firmware upgrade. The controller reports the emu
device `adopted=True, adoption_completed=True, device_upgraded=True`.

**One open problem:** on this seeded image the controller never advances the
device past `state=4` (UPGRADING) to `state=1` (connected). After the upgrade
it re-issues a **new `cfgversion` on every inform** and holds
`wait_for_initial_inform=True` forever. The classic sim (`unifi-network:10.4.57-sim`)
reached `state=1` after an upgrade; this newer UOS-bundled Network app does not.

## Environment / reproduction

- Runtime: docker via colima (aarch64), macOS.
- Real seeded image (published, do NOT hand-build a `dev-seeded`):
  `ghcr.io/jamesbraid/unifi-os-server:seeded`.
- Boot with the documented contract, **`--no-healthcheck`** (see gotcha #2),
  publishing `:443`→11443 and inform `:8080`→18080 (8080 on the host is taken
  by a long-running classic sim `unifi-itest-ctrl`):

```sh
UC=/Users/jamesb/emdash/worktrees/unifi-containers/emdash/ubitofu-bugs-hqxg1
docker rm -f uos-seeded 2>/dev/null
bash "$UC/.github/scripts/run-uos.sh" uos-seeded \
  ghcr.io/jamesbraid/unifi-os-server:seeded \
  --no-healthcheck -p 11443:443 -p 18080:8080
```

- Readiness (unauth, does NOT burn the login budget): poll until
  `GET https://127.0.0.1/api/system` (inside the container) returns 200 AND
  `/var/log/uos-seed-owner.log` contains `ready on :443`. ~30–70 s.
- Run the live test (owner is `admin`/`admin`, seeded by the image):

```sh
DIR=/Users/jamesb/emdash/worktrees/unifi-emu/emdash/unifi-os-tx0wa
UNIFI_EMU_TEST_UOS_INFORM_URL=http://127.0.0.1:18080/inform \
UNIFI_EMU_TEST_UOS_API_URL=https://localhost:11443 \
go test -C "$DIR" -tags integration -run TestEmuAdoptsUOSLive -v .
```

One live run per fresh controller — recreate the container between runs (the
device is already adopted otherwise).

## What is PROVEN

`:443` UOS-native adoption is real and live, contradicting the earlier
"NTP-gate / seeded is DOA" inference:

- `POST :443/api/auth/login` (admin/admin) → HTTP 200 with a full Owner session
  (the seed-owner runs `/api/setup` at boot; healthcheck/seed both succeed).
- The whole handshake works: inform 404→200, `authkey adopted from mgmt_cfg`,
  `adopt` via `/proxy/network/api/s/default/cmd/devmgr` (with the rotating
  CSRF header), upgrade `4.0.21.9965 → 8.6.11.18870` applied.
- The **NTP gate is not the blocker here.** It only fires on the SSO-active
  `-sim` image; the seeded image has SSO disabled and the seed-owner completes
  setup, so `:443` login checks credentials normally.

## Bugs found & fixed / to-fix

1. **`UOSClient.Login` didn't handle HTTP 429 (fixed).** UniFi OS globally
   rate-limits login (`AUTHENTICATION_FAILED_LIMIT_REACHED`). Added backoff +
   retry in `adopt_uos.go`. TODO per owner: replace the hand-rolled loop with
   `hashicorp/go-retryablehttp` (allowed — it is not go-unifi, so no import
   cycle). go-unifi's `ApiClient.loginWithRetry` already does this.
2. **Seeded image healthcheck is self-defeating (unifi-containers bug, NOT
   fixed).** `unifi-os/seeded/docker-healthcheck-seeded.sh` does a full
   `:443` login every 10 s. Login is **globally** rate-limited (proved: a host
   login and an in-container `127.0.0.1` login return 429 at the same instant),
   so the healthcheck starves itself (`health log … 0 0 0 0 1`) and every API
   client. It should poll unauth `/api/system` (the base image's healthcheck
   already does) or reuse a session. Until fixed, run seeded with
   `--no-healthcheck` for any client work.

## THE OPEN PROBLEM: post-upgrade provisioning never finalizes

Instrumented trajectory (poll `stat/device` every 15 s during a live run):

```
t+0   state=7  ver=4.0.21.9965   wait_init=True  cfg=42971e8c   (adopting, pre-upgrade; cfg STABLE)
t+15  state=7  ver=4.0.21.9965   wait_init=True  cfg=42971e8c
t+30  state=4  ver=8.6.11.18870  wait_init=True  cfg=4d4c0125   adopt_done=True upgraded=True
t+45  state=4  ...               wait_init=True  cfg=972b7633   <- new cfgversion
t+60  state=4  ...               wait_init=True  cfg=ee5b11d6   <- every
t+75  state=4  ...               wait_init=True  cfg=5fb82106   <- inform
 ...  ~4 min, ~16 polls, cfgversion different every time, state never leaves 4 ...
```

Facts:
- `cfgversion` is **stable before the upgrade** and **changes on every inform
  after it**. The controller re-pushes config endlessly and never accepts the
  device as reprovisioned.
- `wait_for_initial_inform` stays `True` for the life of the run.
- Controller doc fields at the stall: `adopted=True, adoption_completed=True,
  device_upgraded=True, upgradable=False, version=8.6.11.18870,
  previous_firmware_version=4.0.21.9965, reboot_duration=0,
  adoptable_when_upgraded=False, unsupported=False`.
- The emu's OWN state reaches `CONNECTED` (its state machine flips on the
  post-adopt reply); the gap is purely controller-side `state 4 → 1`.

### What was tried and FAILED (reverted)

Hypothesis: `wait_for_initial_inform` wants the post-reboot "initial inform",
so simulate the reboot by taking the device off the air. Implemented a
`downUntil`/`rebootDowntime` (device silent ~50 s on upgrade/reboot, in
`loop.go`/`device.go`/`response.go`). **Result: still `state=4` after 290 s.**
A reconnect gap alone does not clear it. Reverted (`git checkout HEAD --
loop.go device.go response.go`).

### Leading hypotheses for the drill (unverified)

- The newer bundled Network app expects the device to **confirm the pushed
  `cfgversion`** in a way the emu doesn't, so provisioning never converges
  (cfgversion churn is the primary signal — chase this first).
- The device self-reports `state:2` in the payload (`payload.go`), whose own
  comment says "if a controller stalls here, OpenUniFi falls back to state 4
  … stick with 2 unless an oracle says otherwise." We now HAVE an oracle that
  stalls — worth testing alternate self-reported state / provisioning fields.
- The emu echoes `setstate` tables (`radio_table`/`vap_table`/…). Maybe this
  controller pushes provisioning the emu must apply+confirm differently.

### Recommended next steps

1. Identify the **bundled Network app version** in the seeded image
   (`GET /proxy/network/api/s/default/self` or `/stat/sysinfo`) to scope the
   protocol delta vs 10.4.57.
2. Capture a **real U7PRO's post-upgrade informs** (or read the Network app's
   provisioning code) to see exactly what clears `wait_for_initial_inform` /
   stops the cfgversion churn. Guessing has already cost one failed fix — get
   the ground truth.
3. Only then change `payload.go` / `response.go`.

## Gotchas

1. **Login is globally rate-limited**, not per-IP. Reuse ONE session; don't
   probe login in a poll loop. `stat/device` GETs don't count.
2. Boot seeded with **`--no-healthcheck`** (gotcha above).
3. Host `:8080` is taken by `unifi-itest-ctrl` (classic sim) — use `18080`.
4. The seeded image is **published**: `ghcr.io/jamesbraid/unifi-os-server:seeded`
   / `:5.1.21-seeded`. Don't rebuild a local `dev-seeded`.
5. `api.err.CannotAdopt` on the first few adopt POSTs is normal (young pending
   doc); the test retries.

## Code state (this worktree, uncommitted)

- `adopt_uos.go` — `UOSClient.Login` now retries on 429 (keep; refactor to
  go-retryablehttp).
- `emu_integration_test.go` — new `TestEmuAdoptsUOSLive` + `adopter` interface;
  `adoptAndWaitConnected` controller-side wait bumped to 4 min.
- `loop.go` / `device.go` / `response.go` — reverted to HEAD (reboot-downtime
  experiment removed).
- Container `uos-seeded` left running (11443→443, 18080→8080).
