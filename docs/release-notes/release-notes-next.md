# Loop Client Release Notes

#### New Features

* Instant Out now validates server invoices against a caller-approved maximum
  swap fee.

#### Breaking Changes

* Instant Out requests must now set `max_swap_fee_sat`. Requests that omit the
  fee cap are rejected; an explicit zero cap remains valid. Direct users of
  `Manager.NewInstantOut` must pass the fee cap as a required argument.

#### Bug Fixes

* Cancel reservation creation and its response snapshot read when the manager
  shuts down. Both operations share the request timeout.

* Shared Taproot Asset sweep packets now preserve the destination address
  version and use the required non-interactive split-root layout.

* Instant Out now attempts to cancel server-side swaps when client
  initialization fails, allowing locked reservations to be released without
  waiting for the server timeout.

* Static-address loop-in quotes and manual outpoint initiation now reject
  deposits that are too close to expiry before contacting the Loop server.

* Static Address deposit reconciliation now preserves authoritative
  first-confirmation heights while lnd is catching up, preventing premature
  expiry decisions from mismatched wallet and block-notification heights.

* Loop In commands now parse `--route_hints` as a single JSON array and pass
  every route and hop through unchanged.

* `loopd --version` inside the official Docker images now reports the commit
  it was built from instead of an empty string.

* The official `linux/arm64` Docker images now contain arm64 binaries and an
  arm64 userspace. Every published platform was previously built for amd64, so
  `loopd` failed with `exec format error` on ARM hosts.
  [Issue #1211](https://github.com/lightninglabs/loop/issues/1211)

#### Maintenance

* Document how reservation state entry saves payment choices and handles
  failed validation or writes before an action runs.

* Rename the reservation manager limit to `MaxActiveReservations`.
  Clarify how new-purchase requests create or reuse a reservation.

* Document reservation store guarantees for atomic writes, repeated requests,
  and recovery reads.

* Use reservation names consistently in manager recovery and its tests.
  Document request handling, worker recovery, and shutdown in the manager.

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

* Update Taproot Assets to v0.8.3, taprpc to v1.3.3, and the LND dependency to
  v0.21.3-beta. The minimum Go build version is now 1.25.13.

* The Docker image build now verifies that every platform of the image index
  holds binaries for the architecture it advertises, and gives a release its
  tag only once that check has passed.
  [Issue #1211](https://github.com/lightninglabs/loop/issues/1211)

#### Contributors (Alphabetical Order)
