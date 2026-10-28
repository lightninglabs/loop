# Loop Client Release Notes

#### New Features

* Add an opt-in `fsm/v2` engine with typed transition gates and updates,
  checkpoint-before-effect ordering, revision compare-and-swap, explicit
  restart recovery, and a SQLite/PostgreSQL checkpoint adapter. Existing swap
  flows remain on the original engine pending protocol-specific migration.

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

#### Breaking Changes

#### Bug Fixes

#### Maintenance
- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
