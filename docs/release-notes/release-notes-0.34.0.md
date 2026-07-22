# Loop Client Release Notes

#### New Features

* Static address deposits are now tracked and shown as soon as they appear in
  the wallet, including while they are still in the mempool. Static loop-ins
  can select low-confirmation deposits, with a CLI warning that payment may
  wait for more confirmations under the server's confirmation-risk policy.
  Withdrawals and channel opens continue to require confirmed deposits. The
  selected risk decision and payment deadline are persisted across restarts.
  [PR #1141](https://github.com/lightninglabs/loop/pull/1141)
* `loop openchannel` now supports LND's production `TAPROOT` commitment type
  while retaining explicit support for the legacy `SIMPLE_TAPROOT` type.
  [PR #1156](https://github.com/lightninglabs/loop/pull/1156)

#### Breaking Changes

* `loop openchannel --channel_type=taproot` now requests LND's production
  `TAPROOT` commitment type, which was introduced in LND v0.21. Users who want
  the previous legacy `SIMPLE_TAPROOT` behavior must now specify
  `--channel_type=simple-taproot`.
  [PR #1156](https://github.com/lightninglabs/loop/pull/1156)

#### Bug Fixes

* Static address deposit tracking now reconciles against the wallet, preserves
  deposit identity across RBF replacements, hides replaced or unavailable
  deposits from normal listings, verifies inputs before HTLC signing, and
  rejects duplicate outpoints.
  [PR #1141](https://github.com/lightninglabs/loop/pull/1141)
  [PR #1161](https://github.com/lightninglabs/loop/pull/1161)
* Static loop-in lifecycle handling has been hardened across initialization
  failures, invoice cancellation, HTLC timeouts, shutdown, and restart. Failed
  swaps remain visible, deposits stay locked while an HTLC timeout path is
  active, resumable states are preserved, and failed deposit transitions are
  retried instead of advancing prematurely.
  [PR #1154](https://github.com/lightninglabs/loop/pull/1154)
  [PR #1161](https://github.com/lightninglabs/loop/pull/1161)
  [PR #1141](https://github.com/lightninglabs/loop/pull/1141)
* Slow notification subscribers no longer block the notification manager.
  Required notifications are delivered through ordered per-subscriber queues,
  while optional reservation notifications remain best-effort.
  [PR #1154](https://github.com/lightninglabs/loop/pull/1154)
* Standard loop-in abandonment and timeout handling now classifies
  already-settled invoice errors correctly, reports unexpected invoice
  cancellation failures, and no longer appends a misleading `<nil>` value to
  abandonment errors.
  [PR #1177](https://github.com/lightninglabs/loop/pull/1177)
  [commit 8716a517](https://github.com/lightninglabs/loop/commit/8716a517224e1aaf53d9d27d9f7358e9de980fc4)
* The default `tapd` admin macaroon path now follows the configured Bitcoin
  network while continuing to honor explicit path overrides.
  [commit 37882541](https://github.com/lightninglabs/loop/commit/378825414b4d5df04ff9afbe725cf048c63df9e9)

#### Maintenance

* The Loop Out cost migration now omits payment-hop data that it does not use,
  reducing LND response size and query cost for nodes with large payment
  histories. Deprecation checks were also enabled in CI.
  [PR #1156](https://github.com/lightninglabs/loop/pull/1156)
* Dependencies were updated to address security alerts, PostgreSQL error
  handling was migrated to `pgx` v5, and applicable module Go versions were
  raised to Go 1.25.12.
  [PR #1175](https://github.com/lightninglabs/loop/pull/1175)
* Static loop-in fee validation now logs the HTLC weight, fee rates, computed
  fees, and configured caps to make fee-guard failures easier to diagnose.
  [PR #1158](https://github.com/lightninglabs/loop/pull/1158)
* FSM diagram generation is now deterministic. Static address deposit and
  loop-in diagrams are generated and checked in CI to prevent stale diagrams.
  [PR #1179](https://github.com/lightninglabs/loop/pull/1179)
