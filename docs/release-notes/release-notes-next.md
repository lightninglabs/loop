# Loop Client Release Notes

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

* Asset reservation quotes include an estimated server funding charge in the
  prepaid asset fee. Saved quotes retain their agreed total.

* Probe experimental asset reservations with one full-amount asset hold payment
  per purchase. Verify server receipt and cancellation before approval. Failed
  probes cancel the unpaid purchase; retry with a fresh purchase ID.
  `--skip_probe` remains available when starting a new purchase.

* Filter asset reservations with `asset reservation list --state Ready`.
  Use `--active_only` instead to exclude rejected, canceled and expired
  purchases.
  Startup also excludes completed reservations. List queries read each
  reservation and its latest state together, without loading full histories.

* Display the full prepaid asset-reservation service fee and effective
  percentage, including the transport minimum that enables smaller swaps.

* Add experimental asset-reservation payment adapters, daemon wiring, and CLI
  commands with invoice-based probes, approved BTC prepays, and recovery.
  Purchases require the experimental flag and paired tapd; swaps remain unwired.

#### Breaking Changes

* Shared asset witness builders now require complete, caller-approved virtual
  transfers. Deposit refunds also require the CSV sequence to be set before
  signing any inputs.

#### Bug Fixes

* End unfundable asset reservation purchases immediately with
  `OutOfRange: amount above current maximum`; retain the rejection across
  restarts instead of retrying until funding becomes available.

* Recognize successful reservation hold probes when LND prunes failed HTLC
  attempts. Save routing fee estimates from live payment updates and report
  unavailable estimates separately from observed zero fees after recovery.

* Preserve completed reservation probes across recovery after their deadline,
  and prevent slow status lookups from dispatching a probe after its deadline.

* Allow reservation proof downloads up to the verifier's 16 MiB limit, so
  larger proof histories do not leave paid purchases stuck in verification.

* Cancel reservation creation and its response snapshot read when the manager
  shuts down. Both operations share the request timeout.

* Verify unfamiliar reservation assets by recording verified issuance metadata
  in local tapd before checking the full proof history. No wallet assets are
  imported, and failed proof checks still prevent delivery.

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

* Hold reservation prepays until funding is broadcast. Report funding refusals
  with the existing `OutOfRange: amount above current maximum` error after
  reconciling payment cancellation.

* Group asset reservation JSON fields by identity, quote, payment, probe,
  funding, and lifetime details for easier inspection.

* Use lndclient authentication for reservation payment RPCs while preserving
  the exact payment requests and responses used for recovery.

* Serve asset-reservation RPCs through loopd's existing swap client server
  and assemble the reservation services during daemon initialization. Keep
  their handlers in the existing server file and reuse the daemon's LND clients.

* Document how reservation state entry saves payment choices and handles
  failed validation or writes before an action runs.

* Rename the reservation manager limit to `MaxActiveReservations`.
  Clarify how new-purchase requests create or reuse a reservation.

* Separate switch and select cases in reservation code and tests with blank
  lines for readability.

* Document reservation store guarantees for atomic writes, repeated requests,
  and recovery reads.

* Use reservation names consistently in manager recovery and its tests.
  Document request handling, worker recovery, and shutdown in the manager.

* Document how reservation state machines start new purchases and restore
  saved progress. Flatten quote requests while retaining saved-quote recovery
  and expiry checks. Clarify when actions remain in their current state and
  how probe RPC concurrency is limited. Explain the funding-height checks
  used during delivery verification and monitoring. Format reservation struct
  literals with one field per line.

* Use `SkipProbe` consistently in reservation requests and storage, with one
  saved preference. Payment still requires explicit quote approval.

* Save reservation payment choices with the approved state; remove the
  duplicate quote approval snapshot.

* Accept quoted asset and BTC prepay amounts through the exact quote hash;
  remove duplicate amount limits from reservation approval requests.

* Probe only the estimated reservation swap payment and estimate prepay routing
  fees proportionally. Require a successful probe unless `--skip_probe` is set;
  the flag skips probing for new purchases.

* Add experimental asset reservation proof verification and LND confirmation,
  spend, and expiry tracking. Client readiness follows Instant Out's
  notification-based assumptions; no Bitcoin RPC connection is required.
  Reservation purchases remain experimental.

* Name client purchase states `WaitForDelivery` and `VerifyReservation`.

* Keep reservation lifetime defaults in the server; the client validates
  quoted terms without imposing lifetime minimums.

* Add shared asset reservation terms for the experimental purchase flow.
  Document who supplies each term and how both parties check it.
  Document consent to the quoted asset fee and its BTC prepay equivalent.
* Define a local reservation API with explicit quote approval and probe status.
* Reservation purchases retry failed probes in the background and preserve
  recovery instructions after an ambiguous approval reply. Proof verification
  applies the shared kit's provenance checks; single-reservation reads use a
  consistent database snapshot.

* Add experimental `loop asset reservation` purchase and inspection commands.
  Expose `buy`, `list`, and `get`; unapproved purchases expire automatically.
  Cancellation stays internal, with no public cancellation command or local RPC.
  Validate quoted amounts without imposing a client-side pricing policy.
  Name the saved funding depth `required_confirmations`; the server defaults
  to three.
  Asset Loop Out execution is not enabled.

* Define and wire the experimental asset reservation RPC contract.
  Describe public reservation status and owned get/list queries.
  Clarify the receiving node and conversion peer in reservation quotes.

* Update Taproot Assets to v0.8.3, taprpc to v1.3.3, and the LND dependency to
  v0.21.3-beta.

#### Contributors (Alphabetical Order)
