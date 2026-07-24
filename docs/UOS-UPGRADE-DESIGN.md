# UniFi OS AP upgrade-cycle design

## Problem

The seeded UniFi OS controller adopts the emulated U7 Pro and sends an
`upgrade` command. The emulator changes its firmware version, but the
controller remains in `state=4` and generates a new `cfgversion` on each
inform.

Two protocol differences caused the stall:

- The emulator reported device-side `state=2` as soon as it had an adoption
  key. OpenUniFi and the controller expect an adopted device to report `4`.
  This is not the controller REST state: a healthy device continues sending
  `4` while its `stat/device` record settles at state `1`.
- More importantly, the emulator ignored `use_aes_gcm=true` in `mgmt_cfg`
  and kept emitting CBC-encrypted informs. The newer controller treats that
  as pre-provisioning traffic, sends another management config, and assigns
  a new cfgversion before its normal post-upgrade path can run.

## Approaches

1. Report `state=4` for every adopted inform. This matches OpenUniFi and is
   the selected approach.
2. Track a temporary provisioning phase and then return to device-side
   `state=2`. A live seeded-UOS run disproved this: the firmware upgrade
   completed, but `wait_for_initial_inform` stayed true and `cfgversion`
   churn resumed as soon as the emulator returned to `2`.
3. Suppress the controller-requested upgrade or alter its database state.
   This would make the test green without emulating the device lifecycle and
   violates the repository's inform-only rule.

## State and data flow

The emulator keeps the existing public states: `PENDING`, `ADOPTING`, and
`CONNECTED`. The wire and public state machines remain deliberately separate:

- Pending devices send protocol `state=1`.
- Adopted devices send protocol `state=4`, including after the emulator's
  public state reaches `CONNECTED`.
- The first management response arrives over CBC. When it contains
  `use_aes_gcm=true`, all later informs use the TNBU GCM flag and authenticate
  the complete 40-byte header as additional data. Controller responses may
  use either mode; the decoder accepts both.
- `setparam` and `setstate` persist the controller's `cfgversion`; the next
  inform echoes it while remaining at protocol state `4`.
- `upgrade` changes firmware and resets uptime; the next adopted inform
  carries the new version at protocol state `4`.

Controller REST state remains separate: the live test still succeeds only
when `stat/device` reports `state=1, adopted=true`.

## Tests and evidence

Unit tests pin the payload states and encryption transition:

- pending payload uses protocol `state=1`
- every adopted payload uses protocol `state=4`
- `use_aes_gcm=true` switches the next inform to AES-GCM
- pushed `cfgversion` is echoed without changing the adopted wire state
- upgrade: firmware and uptime change, the wire state remains `4`,
  and adoption credentials survive

The integration test boots a fresh
`ghcr.io/jamesbraid/unifi-os-server:seeded` controller, adopts one U7 Pro
through the UOS proxy API, accepts the firmware upgrade to
`8.6.11.18870`, and waits for controller `state=1, adopted=true`. The
controller document and emulator log are the completion evidence.
