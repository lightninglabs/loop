# Loop Client Release Notes

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

#### Breaking Changes

#### Bug Fixes

* Shared Taproot Asset sweep packets now preserve the destination address
  version and use the required non-interactive split-root layout.

#### Maintenance
- Update the LoopRPC requirement to the revision used by this release.

* Update Taproot Assets to v0.8.3, taprpc to v1.3.3, and the LND dependency to
  v0.21.3-beta.

#### Contributors (Alphabetical Order)
