# UniFi OS AP upgrade-cycle design

## Problem

The seeded UniFi OS controller adopts the emulated U7 Pro and sends an
`upgrade` command. The emulator changes its firmware version, but the
controller remains in `state=4` and generates a new `cfgversion` on each
inform.

The device-side protocol state is the mismatch. The emulator reports
`state=2` as soon as it has an adoption key. OpenUniFi and the controller's
own provisioning code expect an adopted device to report `state=4` while it
applies the pushed configuration. The device may report steady-state
`state=2` only after it has sent the controller's expected `cfgversion`.

## Approaches

1. Report `state=4` for every adopted inform. This matches the OpenUniFi
   reference and is a useful diagnostic, but it never models the end of
   provisioning.
2. Track configuration acknowledgement. Report `state=4` during adoption,
   upgrade, and provisioning. After an inform carries the last pushed
   `cfgversion` without receiving a replacement, report `state=2`. This is
   the selected approach because it models both sides of the transition.
3. Suppress the controller-requested upgrade or alter its database state.
   This would make the test green without emulating the device lifecycle and
   violates the repository's inform-only rule.

## State and data flow

The emulator keeps the existing public states: `PENDING`, `ADOPTING`, and
`CONNECTED`. It adds private provisioning state to each device:

- `pendingCfgversion` is set when `setparam` or `setstate` supplies a new
  configuration version.
- Pending devices send protocol `state=1`.
- Adopted devices send protocol `state=4` while adopting, after an upgrade,
  or while `pendingCfgversion` has not been acknowledged.
- An HTTP 200 inform carrying `pendingCfgversion` acknowledges that version.
  If the response supplies no newer version, the next inform uses protocol
  `state=2`.
- A later `setparam`, `setstate`, or `upgrade` returns the device to protocol
  `state=4` until the same acknowledgement rule completes again.

Controller REST state remains separate: the live test still succeeds only
when `stat/device` reports `state=1, adopted=true`.

## Tests and evidence

Unit tests pin the payload states and the acknowledgement boundary:

- pending payload uses protocol `state=1`
- adopted but unprovisioned payload uses protocol `state=4`
- pushed `cfgversion`: one inform at protocol `state=4`, followed by
  steady-state `state=2` when no replacement arrives
- upgrade: firmware and uptime change, provisioning returns to `state=4`,
  and adoption credentials survive

The integration test boots a fresh
`ghcr.io/jamesbraid/unifi-os-server:seeded` controller, adopts one U7 Pro
through the UOS proxy API, accepts the firmware upgrade to
`8.6.11.18870`, and waits for controller `state=1, adopted=true`. The
controller document and emulator log are the completion evidence.
