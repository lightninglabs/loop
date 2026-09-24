# Loop Client Release Notes

#### New Features

* Share asset deposit, reservation and CSV/CLTV HTLC contracts through
  `swapkit`, with transfer, signing, proof and recovery helpers backed by
  `github.com/lightninglabs/tap-sdk` v0.1.1 and Taproot Assets v0.8.8. New on-chain
  asset swaps require tapd v0.8.8 or later.

* Add experimental asset-reservation payment adapters with invoice-based
  probes, approved BTC prepays, and node-bound recovery. Runtime access remains
  disabled pending service wiring and integration tests.

#### Breaking Changes

#### Bug Fixes

* The `postgres.maxconnections` option now limits the number of open
  connections to the Postgres server. It was ignored before, so concurrent
  requests could open more connections than the server accepts. If the option
  isn't set, loopd now opens at most 25 connections.

#### Maintenance

* Document how reservation state entry saves payment choices and handles
  failed validation or writes before an action runs.

* Document reservation store guarantees for atomic writes, repeated requests,
  and recovery reads.

* Use `SkipProbe` consistently in reservation requests and storage, with one
  saved preference. Payment still requires explicit quote approval.

* Save reservation payment choices with the approved state; remove the
  duplicate quote approval snapshot.

* Accept quoted asset and BTC prepay amounts through the exact quote hash;
  remove duplicate amount limits from reservation approval requests.

* Name client purchase states `WaitForDelivery` and `VerifyReservation`.

* Keep reservation lifetime defaults in the server; the client validates
  quoted terms without imposing lifetime minimums.

* Add shared asset reservation terms for the experimental purchase flow.
  Document who supplies each term and how both parties check it.
  Document consent to the quoted asset fee and its BTC prepay equivalent.
* Define a local reservation API with explicit quote approval and probe status.
  Validate quoted amounts without imposing a client-side pricing policy.
  Name the saved funding depth `required_confirmations`; the server defaults
  to three.
  This does not enable reservation purchases or asset Loop Out.

* Define the experimental asset reservation RPC contract. Service registration
  and real-node adapters remain disabled.
  Describe public reservation status and owned get/list queries.
  Clarify the receiving node and conversion peer in reservation quotes.

#### Contributors (Alphabetical Order)

* Bound experimental reservation quote acquisition to one minute, reject invalid
  quotes before payment, and show the exact BTC prepay and implied prices before
  approval. Reject prepay/probe RFQ rates differing by more than 5%.
* Keep ordinary swaps available if experimental reservation recovery fails, and
  avoid repeated full proof verification and idle-watch error logs for Ready
  reservations.
