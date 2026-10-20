# Loop Client Release Notes

#### New Features

* Asset static loop-ins and their listings use the existing static-address
  RPCs with explicit native asset fields and durable request IDs.

* Wire native asset deposits and static loop-ins into normal `loopd` and
  `loop static asset` commands, with durable request identities, shared
  Bitcoin deposit exclusion, startup recovery, and original CSV refunds.
  The separate authenticated transaction runtime retains wallet signing
  and proof state across process restarts.

* Add a durable client execution controller for server-preimage asset static
  loop-ins, with recovery bundles persisted before signatures, independent
  local payout checks, and confirmed asset-proof gates for deposit release.
  Bitcoin and asset flows share protocol-selected Loop In and deposit FSM
  transition policies, with explicit protection after claim exposure.

* Add Loop-owned `swapkit` contracts and a separate `swapkit/tapsdk` adapter
  using the generic Tap SDK builder for asset deposits and HTLCs.

#### Breaking Changes

#### Bug Fixes

* Resume asset recovery chain scans across deadlines and process restarts,
  invalidate scan checkpoints after reorgs, and refresh re-confirmed refund
  proofs before invoice cancellation and server reporting.

#### Maintenance

#### Contributors (Alphabetical Order)
