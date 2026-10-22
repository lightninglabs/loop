# Loop Client Release Notes

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

* Add shared asset swap contracts for CSV deposits, CLTV reservations and
  CSV/CLTV HTLCs in both swap directions, with native Tap SDK construction,
  signing, proof and recovery helpers in `swapkit/tapsdk`. Approval binds the
  negotiated asset and Bitcoin terms, requires SIGHASH_DEFAULT, and checks
  bounded proof histories and current funding. New CLTV refunds signal RBF;
  saved transactions retain their original constraints. Asset-aware witness
  estimates include the full control blocks. The existing NUMS/single-OP_TRUE
  asset key policy is preserved.

#### Maintenance

* Align the btcd v2 foundation through immutable migration PR sources,
  retaining Taproot Assets v0.8.6 fixes and Go 1.27.3. Pin lnd and its nested
  modules to master revision `0d9c9d3afb27`; the runtime RPC minimum remains
  separate. The SDK source includes exact-outpoint proof selection,
  received-asset signing metadata, and proof activation API compatibility.
  The LiT compatibility source preserves SQL error classification while
  passing Go 1.27's error-wrapping vet check.

- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
