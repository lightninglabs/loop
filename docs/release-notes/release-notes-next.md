# Loop Client Release Notes

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

#### Breaking Changes

#### Bug Fixes

* Fix a connection ownership bug where an embedded loop daemon closed the
  shared lnd connection on shutdown. When running as a subserver of
  lightning-terminal, a graceful stop or a runtime error in the loop daemon
  tore down the lnd connection owned by the parent process, breaking lnd
  access for LiT and all other integrated subservers. The daemon now tracks
  whether it created the connection itself and only closes owned connections.

* Bound the static Loop In risk decision replay cache to prevent unbounded
  memory growth from swap server notifications for unknown swaps. Cached
  decisions retain only a copy of the validated swap hash, without unknown
  protobuf fields or oversized backing buffers.

#### Maintenance
- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
