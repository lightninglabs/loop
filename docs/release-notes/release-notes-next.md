# Loop Client Release Notes

#### New Features

* Add an opt-in, storage-agnostic `fsm/v2` engine with typed transition gates
  and updates, persist-before-effect ordering, revision compare-and-swap, and
  explicit restart recovery. Existing swap flows remain on the original engine
  pending per-flow adoption.

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

#### Maintenance
- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
