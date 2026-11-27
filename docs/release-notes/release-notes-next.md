# Loop Client Release Notes

#### New Features

* Share asset deposit, reservation and CSV/CLTV HTLC contracts through
  `swapkit`, with transfer, signing, proof and recovery helpers backed by
  `github.com/lightninglabs/tap-sdk` v0.1.1 and Taproot Assets v0.8.9. New on-chain
  asset swaps require tapd v0.8.9 or later. Shared helpers also authenticate
  group issuance evidence and register issuance metadata before verification.

* Add `swapkit.LoopInTimelocks` and `swapkit.LoopOutTimelocks`, which check
  the ordering of deposit, reservation, HTLC and Lightning payment expiries
  against the current tip before a swap flow pays, signs or reveals a preimage.

* The asset Loop In fallback HTLC transaction built by `swapkit/tapsdk` no
  longer fixes its fee when the swap is signed. It is now a zero-fee version 3
  transaction with a pay-to-anchor output, so its publisher sets the fee at
  broadcast time with a child transaction and submits both as one package.

* Add `tapsdk.HTLCRequest` and `tapsdk.ValidateHTLCPackage`, which build and
  validate that zero-fee pay-to-anchor HTLC transaction for either direction:
  from a client deposit for asset Loop In, or from a server reservation for
  asset Loop Out.

* Add `tapsdk.ValidateCooperativeSpend`, the funder's check of a cooperative
  spend of its deposit or reservation before it co-signs.

#### Breaking Changes

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

* Harden the shared `swapkit` adapter against hostile peer input: bind a
  recovery fallback to the authenticated deposit before co-signing, validate
  malformed group and alt-leaf proofs before reuse, verify a caller-provided
  asset witness before declaring it, recognize annex spends, and report a
  reorg during outcome verification as retryable.

#### Maintenance

* Update the OpenTelemetry Go SDK from v1.45.0 to v1.47.0 in Loop and
  `looprpc`.

#### Contributors (Alphabetical Order)
