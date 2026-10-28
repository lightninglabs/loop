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

* Make static-address withdrawals durable with an authoritative workflow,
  atomic deposit ownership, signed transaction persistence before publication,
  per-state restart recovery, and retained fee-bump attempts. Import legacy
  withdrawals into a separate domain schema without changing the old tables;
  ambiguous legacy data is quarantined. Existing withdrawal RPCs and the
  listing response remain compatible, with listings read from the new store.

#### Maintenance
- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
