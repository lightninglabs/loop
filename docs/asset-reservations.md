# Asset reservations

Status: a fresh, local implementation on `hai/asset-reservations-stepwise`,
based on the shared asset kit at `81e876cd`. Reservation purchase comes first;
asset Loop Out will build on it. Purchases require `--experimental` and
`--tapd.activate` in standalone loopd. They are not production-ready yet.

The real-node reservation case passed on 2026-09-10 with tapd v0.8.3 and
LND v0.21.3-beta: a BTC-only payer, both probes, purchase through Ready,
client/server restarts, rejected proofs, insufficient-funds cancellation,
and the full 1,440-block CSV sweep. This run uses a SQLite client. Node-process
fault injection, client PostgreSQL/Neutrino variants, and the broader failure
matrix remain; swap execution is not part of this case. That run predates the
single-probe flow described below.

The full-amount hold-probe case passed on 2026-09-23 with a BTC-only payer:
10,000 asset units reached the receiving asset channel, the server canceled
the hold invoice, and purchase completed after restarts. The case also covered
unpaid cancellation, recovery after funding inventory was restored, proof
verification, and the shortened 144-block CSV sweep. Client/server unit tests
cover interrupted probing, receipt persistence, and cancellation recovery.

## Commands

`loop asset reservation buy --asset_id <hex> --amt <units>` requests a quote,
probes the full reserved asset amount, displays its delivery result and
the terms, and asks before paying. Normal approval requires a successful main
probe, though success does not reserve liquidity for the later swap.
The prepay routing fee estimate scales the main fee by the BTC payment amounts,
rounds up to a millisatoshi, and ignores fixed hop fees. Failed probes have no
fee estimate. The actual prepay routing limit remains the conventional Loop Out
limit: 10 sats plus 2%. `--max_routing_fee` sets an explicit limit in sats;
`--yes` skips the approval prompt, but still requires a successful probe.

If the server lacks available asset inventory or Bitcoin funding fees, the
purchase ends immediately in `QuoteRejected`. The CLI displays
`cannot initiate swap: rpc error: code = OutOfRange desc = amount above current maximum`.
No invoice is paid and neither polling nor restart retries that purchase.
Use a fresh purchase ID to try again after funds become available. Already paid
purchases continue delivery recovery if inventory later disappears.

`--skip_probe` skips probing for a new purchase and permits approval without a
successful main probe. It still displays the terms and asks before paying,
unless `--yes` is also set. Fee estimates are unavailable when no probe ran.
A failed or timed-out probe cancels the unpaid purchase, including after
restart. LND may try alternative routes within the single payment. Request a
new purchase with a fresh reservation ID to try again, optionally with
`--skip_probe`. The canceled purchase cannot be approved or revived. Its quote
and payment facts remain saved; recovery finishes any outstanding cancellation.

`list` shows saved purchases. `get <txid:vout>` inspects a funded reservation.
Before funding, use `get --reservation_id <hex>` with the ID printed before
the initial request. Repeating `buy` with that ID and the same asset and amount
resumes the purchase without another invoice. Probing and cancellation are
internal; the public commands are `buy`, `list`, and `get`.

Closing the CLI does not cancel daemon work. A declined prompt leaves the
purchase unpaid and saved until its quote expires. After approval, closing
the CLI does not stop payment or delivery. The later swap and its
execution-time requoting are not part of these purchase commands.

## Implementation

The [server reservation RPC contract](asset-reservation-rpc.md) defines quote,
status, list, proof retrieval, and unpaid cancellation. Its protobuf service
is separate from Bitcoin reservations. The local purchase API is registered
behind loopd's existing macaroon permissions.

Implemented so far: shared terms, checked amount validation, SQL tables, and a
typed SQLite/PostgreSQL store. Creation saves terms and the first state in one
transaction. Repeating the same purchase ID returns saved progress; changing
its terms or keys fails. Updates preserve the agreed fee and append state
history. The store retains payment and delivery facts as well. Startup restores
unfinished purchases before local RPC admission.

Client purchase columns retain the quote, probes, payment choices, payment,
and proof. Before a quote, the fee and lifetime remain unset. The typed store
accepts one immutable quote. Approving its hash accepts the asset fee and BTC
prepay amount. The client saves the prepay routing cap and `SkipProbe` with
the transition to `PayPrepay`. That state records consent; the quote amounts
need no second snapshot. The store preserves these choices, the paying node
and request, terminal payment result, outpoint, credit, and proof. Repeated unchanged writes
do not append state history.

The client [FSM](../assets/reservation/fsm.md) now saves and probes the quote,
waits for explicit approval, tracks the exact prepay, and verifies delivery
through narrow payment and wallet interfaces. Its SQLite-backed tests cover
restarts, failed writes, cancellation racing settlement, proof rejection, and
CSV expiry. The asset adapter now verifies full proofs through tapd and the
shared deposit kit, and tracks confirmations and spends through LND. The
payment adapter validates both receiving RFQs and invoices, probes through
LND's `SendPaymentV2`, and sends the approved BTC prepay. It saves the paying node
and exact request before dispatch. Runtime wiring uses the existing LND
connection and its service macaroons; it opens no Bitcoin backend connection.

Prepays use one payment part to avoid conversion-rounding losses. The client
checks available BTC in one channel, including its reserve and the approved
routing cap. Skipping the probe cannot bypass these checks. An
uncertain send is resolved by tracking the saved hash on the saved node, not
by paying again.

The manager restores every live purchase and serializes its actions in one
worker. New-purchase limits never suppress recovery. Exact request retries
reuse saved keys and terms. Unapproved quotes expire through internal
cancellation and payment reconciliation.
RPC calls and periodic checks are sufficient for this version. The existing
notification stream is not required or connected. Shutdown cancels pending
node calls and waits for workers to finish; status readers use the store.

The shared lifetime calculation derives timeout and execution heights from
the original funding confirmation. The server checks the quoted depth and
initial usable window before declaring the reservation Ready. These delivery
checks live in the server's reservation package; proof verification and spend
tracking remain separate responsibilities.

A client returning late uses the original confirmation and CSV clock, not a
new delivery window. Its Ready state means verified and unexpired; the later
swap must also check the execution cutoff and its own claim margin.

One reservation holds one asset and amount in one Bitcoin output. The server
owns the lifetime defaults: a 1,440-block CSV, three confirmations, a 90-block
execution margin, and 1,000 initially usable blocks. The client checks that
the quoted values are positive and fit the CSV lifetime; it does not enforce
its own lifetime minimums. Before marking a reservation Ready, it checks the
quoted confirmation depth, full asset proof, exact output and scripts, and
local keys. Like Instant Out, a ready client reservation is not known spent
or expired; it is not a synchronous UTXO assertion.

For assets new to the client, the adapter first submits the history's issuance
proof to local tapd. Tapd verifies it and records the metadata needed by v0.8.3
to return a decoded transfer proof. This does not import wallet assets. Full
history verification is still required, and either RPC failure stops delivery.
The local tapd credentials and universe policy must allow issuance insertion.

`RequiredConfirmations` (`required_confirmations` in SQL) saves the agreed
funding depth, not a live count. The server defaults to three. Recovery preserves
the agreed depth; the CSV lifetime still starts at the first confirmation.

The server starts with a base fee of 0.1% of the asset amount (rounded up to an
indivisible unit), configurable on the server. It adds conventional fast Loop
Out's funding estimate at the reservation's configured funding fee rate,
converted to asset units at the prepay RFQ rate and rounded up. The default
5 sat/vbyte prices the 153-vbyte estimate at 765 sats. The total must also meet
the receiving RFQ's transport minimum, with conversion rounding checked against
the outgoing HTLC's BTC anchor. The entire fee is prepaid. The CLI displays it
in asset units and its effective percentage before approval. The server receives
assets through an independent edge; the client can pay BTC.
The prepay is an ordinary invoice. Its exact settled asset receipt permits
funding preparation. The separate full-amount probe uses a hold invoice that
is canceled without settlement; it never grants prepay credit or locks funding
inputs.

The later swap fee equals the quoted prepay. The exact settled prepay credits
it once, so the main asset invoice equals the reserved amount. There is no
separate prepay refund: the credit prevents charging the total fee twice, and
the server retains the funding charge. An unused reservation loses only its
prepay, not its principal. The funding charge is an estimate, not an adjustment
to the actual transaction fee. The server pays miners from its BTC wallet;
receiving asset fees does not automatically convert them into BTC. This estimate
does not add timeout sweep costs.
The later BTC conversion quote can change and requires client approval.
The principal must independently clear the transport minimum, both in the
purchase-time estimate and at execution's fresh rate. Raising the service fee
does not make a below-minimum principal transportable. Near that minimum the
fee can approach or exceed the principal; it is still fully charged and is
not a refundable advance against principal. No maximum fee percentage is
imposed by this policy.

As in conventional Loop Out, the server offers a price and the client checks
it against its own limits. Save and display the quoted terms before asking
for approval. Shared validation and SQL check positive
amounts, overflow, and lifetime, not a fixed fee percentage. Recovery uses the
accepted fee; it never recalculates it from a later pricing policy.

Each quote registers one full-amount asset hold probe. Its payment hash is
`SHA256(reservation_id)` with the low bit of the first byte flipped, as in
conventional Loop In probing. Neither party knows a settling preimage. The
client saves its paying node, request, and deadline before sending. The server
validates the accepted asset ID, amount, RFQ, and channel, saves the receipt,
then cancels. Success requires that receipt, invoice cancellation, and a local
failed payment with `FAILURE_REASON_INCORRECT_PAYMENT_DETAILS`. Quote status reports receipt and cancellation
separately. `CancelAssetReservation` with `probe_only` releases just the probe,
leaving a successful or explicitly skipped purchase available for approval.

Timeout cancels the unpaid purchase and requests receiver cancellation; it
does not release an in-flight HTLC by itself. Recovery tracks and cleans up the same
payment. Failed probes never automatically start over. The initial implementation
uses one edge and one payment part, preserving exact asset-unit conversion.

The client keeps the `SendPaymentV2` stream open and saves fees from in-flight
routes before LND prunes failed attempts. Recovery uses `TrackPaymentV2` with
in-flight updates enabled. Success depends on the payment-level failure reason,
not retained attempt records; `keep-failed-payment-attempts` is not required.
If recovery misses the live route, the probe can still succeed, with fee
estimates marked unavailable. A saved availability flag distinguishes that
case from an observed zero-fee route. The most recently observed route supplies
an estimate; LND may retry using another route. The approximate prepay estimate remains
`ceil(main routing fee * prepay BTC amount / main probe BTC amount)`, ignoring
fixed hop fees. Neither estimate reserves liquidity or the later swap's rate.

Keep the purchase and future swap FSMs separate, with static-address-style
actions, managers, SQL stores, state history, and `OnRecover`. Persist payment
intent and exact publication data before their external calls. Recover by
checking stored facts against LND, tapd, and the chain. No versioned recovery
envelopes, generic effects framework, or legacy fee-refund states.

Reuse Instant Out's LND confirmation, spend, and block notifications, restoring
subscriptions with saved height hints. Do not require wallet imports, a direct
Bitcoin RPC connection, or a pre-publication watch handshake. No custom block
scanner or scan cursor is needed. A quiet spend stream is not a synchronous
unspent guarantee. The later swap keeps its own payment safety checks.

Build in small commits: terms; SQL; stores; FSM actions; manager; asset adapter;
payment adapter; RPC; daemon wiring; CLI; integration tests. Keep tests with
their owning change. Do not migrate the old prototype's databases.

Standalone `loop asset reservation buy/list/get` will precede swap execution.
Funded reservations use canonical outpoints in commands; stable internal IDs
cover pending purchases and recovery. Later, `loop asset out --outpoint` uses
an existing reservation without buying another or charging prepay twice.

Acceptance requires real-node purchase, proof verification, cancellation of
unfundable held payments, restart without duplicate collection/publication,
delayed confirmation, and automatic server CSV sweeping. Preserve existing
asset and BTC flows. Reorg recovery, fee bumping, and tap-sdk migration remain
separate work; keep this experimental feature disabled for production.

The implemented purchase path bounds quote acquisition to one minute across
restarts and stops invalid quotes before payment. Approval shows the exact BTC
prepay and implied price, with a 5% consistency bound between the prepay and
probe RFQ rates. Reservation recovery runs independently of ordinary swaps.
Ready monitoring re-verifies saved proof once after restart, then watches chain
state without repeatedly invoking full proof verification.
