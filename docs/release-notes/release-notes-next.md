# Loop Client Release Notes

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

#### Breaking Changes

* Shared asset witness builders now require complete, caller-approved virtual
  transfers. Deposit refunds also require the CSV sequence to be set before
  signing any inputs.

#### Bug Fixes

* Shared asset sweeps validate asset conservation and Bitcoin output commitments,
  including change and passive assets, before signing. New deposit addresses use
  explicit asset V1 and address V1 versions, refund signing preserves existing
  signatures, and returned control blocks no longer expose mutable kit keys.

* Deposit proof verification now requires genesis-rooted histories and rejects
  ownership-only proofs, including those hidden in additional inputs.

* Shared asset kits now isolate proof and RPC buffers, reject oversized proofs
  and unencodable amounts, and enforce distinct x-only signing keys.

* Shared Taproot Asset sweep packets now preserve the destination address
  version and use the required non-interactive split-root layout.

#### Maintenance
- Update the LoopRPC requirement to the revision used by this release.

* Keep reservation lifetime defaults in the server; the client validates
  quoted terms without imposing lifetime minimums.

* Add shared asset reservation terms for the experimental purchase flow.
  Document who supplies each term and how both parties check it.
  Validate quoted amounts without imposing a client-side pricing policy.
  Name the saved funding depth `required_confirmations`; the server defaults
  to three.
  This does not enable reservation purchases or asset Loop Out.

* Define the experimental asset reservation RPC contract. Service registration
  and real-node adapters remain disabled.
  Describe public reservation status and owned get/list queries.
  Clarify the receiving node and conversion peer in reservation quotes.

* Update Taproot Assets to v0.8.3, taprpc to v1.3.3, and the LND dependency to
  v0.21.3-beta.

#### Contributors (Alphabetical Order)
