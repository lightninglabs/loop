# Loop Client Release Notes

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

#### Breaking Changes

#### Bug Fixes

* Bound the static Loop In risk decision replay cache to prevent unbounded
  memory growth from swap server notifications for unknown swaps. Cached
  decisions retain only a copy of the validated swap hash, without unknown
  protobuf fields or oversized backing buffers.

* Static address deposits now stay linked to the loop-in that spent them.
  If a loop-in's htlc confirmed after its payment deadline, and other
  loop-ins had selected its deposits in the meantime, the deposits were
  listed under those loop-ins and the first loop-in's on-chain cost was
  missing. loopd also repairs existing deposits at startup.

#### Maintenance
- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
