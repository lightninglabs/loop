# Loop Client Release Notes

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

#### Breaking Changes

#### Bug Fixes

* Select Taproot Assets' bounded TLV decoder in the consuming modules so a
  malformed asset proof returns an error before allocating from attacker-supplied
  TLV lengths.

#### Maintenance

* Update Taproot Assets to v0.8.6, taprpc to v1.3.3, and the LND dependency to
  v0.21.3-beta, retaining upstream's Go 1.27.3 build requirement.

- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
