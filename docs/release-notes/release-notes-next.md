# Loop Client Release Notes

#### New Features

#### Breaking Changes

* `loop static deposit` now funds a fresh address from loopd's connected lnd
  wallet. Pass `--amt` or `--sweepall`. The new `FundStaticAddress` RPC uses
  the existing `swap:execute` and `loop:in` permissions. Loopd's lnd macaroon
  must authorize `SendCoins` (`onchain:write`). Scripts that only need an
  address can use `loop static new`. The funding RPC limits fee rates to
  100 sat/vbyte and checks for a deposit of at least 15,000 sat. A sweep
  checks the expected amount after fees and the channel reserve.

#### Bug Fixes

* Fix a deadlock in the static address loop-in, withdrawal and open channel
  managers. If the caller of a request stopped waiting before the manager
  answered, for example because the `loop` command was interrupted, the
  manager blocked on its response and served no further requests of that
  kind until `loopd` was restarted.

* Fix a hang in the recovery of unfinished loop outs. If the server reported
  two unfinished swaps in quick succession, the recovery stopped for good
  until `loopd` was restarted. A recovery attempt that is retried after a
  failure also no longer outlives `loopd`'s shutdown.

* SQLite write transactions now take the database write lock when they begin.
  Before, a transaction that read before it wrote failed at once with
  `database is locked (SQLITE_BUSY)` if another write was in progress, instead
  of waiting for the busy timeout. For example, loopd could log this error when
  it stored the server's confirmation risk decision for a static Loop In.

#### Maintenance

* Update the OpenTelemetry Go SDK from v1.45.0 to v1.47.0 in Loop and
  `looprpc`.

#### Contributors (Alphabetical Order)
