# Asset reservations

Status: a fresh, local implementation on `hai/asset-reservations-stepwise`,
based on the shared asset kit at `81e876cd`. Reservation purchase comes first;
asset Loop Out will build on it. No runtime feature is enabled yet.

The [reservation RPC contract](asset-reservation-rpc.md) defines quote,
status, list, proof retrieval, and unpaid cancellation. Its protobuf service
is separate from Bitcoin reservations and is not registered at runtime yet.

Implemented so far: shared terms, checked amount validation, SQL tables, and a
typed SQLite/PostgreSQL store. Creation saves terms and the first state in one
transaction. Repeating the same purchase ID returns saved progress; changing
its terms or keys fails. Updates preserve the agreed fee and append state
history. The store now retains payment and delivery facts as well. Runtime
wiring is still separate work.

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
shared deposit kit, and tracks confirmations and spends through LND. Payment
adapters and runtime wiring remain separate work.

The manager restores every live purchase and serializes its actions in one
worker. New-purchase limits never suppress recovery. Exact request retries
reuse saved keys and terms; cancellation retries return the saved outcome.
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

The initial server policy quotes a prepay of 0.1% of the asset amount,
rounded up to an indivisible unit. The server receives assets through an
independent edge; the client can pay BTC.
It is a hold invoice: accepted payment permits funding preparation, then the
server checks funds and expiry, settles, and publishes the saved transaction.
Probing does not pay that invoice or lock funding inputs.

The later swap fee equals the quoted prepay. The exact settled prepay credits
it once, so the main asset invoice equals the reserved amount. There is no
separate prepay refund. An unused reservation loses only its prepay, not its
principal.
The later BTC conversion quote can change and requires client approval.

As in conventional Loop Out, the server offers a price and the client checks
it against its own limits. Save and display the quoted terms before asking
for approval. Shared validation and SQL check positive
amounts, overflow, and lifetime, not a fixed fee percentage. Recovery uses the
accepted fee; it never recalculates it from a later pricing policy.

Use LND's invoice-based fee estimator only with the probe-only invoice for the
estimated main payment. Derive the approximate prepay routing fee as
`ceil(main routing fee * prepay BTC amount / estimated main BTC amount)`.
Show the main probe result and any available fee estimates before approval. The actual
prepay still requires invoice validation and obeys the approved routing cap.

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
