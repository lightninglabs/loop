# Loop Client Release Notes

#### New Features

* Add experimental same-group asset deposits and Loop In through `loopd`,
  `loop static asset`, and typed fields on existing static-address RPCs.
  The normal deposit/Loop In managers and SQL stores own admission and
  recovery. Server-controlled execution requires locally verified asset
  payments; reservations remain protected until confirmed proof resolution.

* Add Loop-owned deposit/HTLC contracts and a Tap SDK transaction runtime
  with durable wallet signing, proof recovery and original CSV refunds.
  The separate authenticated process preserves signing state across restarts.

#### Breaking Changes

#### Bug Fixes

* Resume asset recovery chain scans across deadlines and process restarts,
  invalidate scan checkpoints after reorgs, and refresh re-confirmed refund
  proofs before invoice cancellation and server reporting.

#### Maintenance

#### Contributors (Alphabetical Order)
