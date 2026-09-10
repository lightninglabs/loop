# Asset reservations

Status: a fresh, local implementation on `hai/asset-reservations-stepwise`,
based on the shared asset kit at `81e876cd`. Reservation purchase comes first;
asset Loop Out will build on it. No runtime feature is enabled yet.

The [reservation RPC contract](asset-reservation-rpc.md) defines quote,
status, list, proof retrieval, and unpaid cancellation. Its protobuf service
is separate from Bitcoin reservations and is not registered at runtime yet.

One reservation holds one asset and amount in one Bitcoin output. It expires
1,440 blocks after funding confirmation. The client checks three confirmations,
the full asset proof, exact output and scripts, local keys, and unspent status
using LND and tapd before marking it Ready.

`RequiredConfirmations` (`required_confirmations` in SQL) saves the agreed
funding depth, not a live count. Its default is three. Recovery preserves the
agreed depth; the CSV lifetime still starts at the first confirmation.

The prepay is 0.1% of the asset amount, rounded up to an indivisible unit. The
server receives assets through an independent edge; the client can pay BTC.
It is a hold invoice: accepted payment permits funding preparation, then the
server checks funds and expiry, settles, and publishes the saved transaction.
Probing does not pay that invoice or lock funding inputs.

The later swap fee is the same 0.1%. The exact settled prepay credits it once,
so the main asset invoice equals the reserved amount. There is no separate
prepay refund. An unused reservation loses only its prepay, not its principal.
The later BTC conversion quote can change and requires client approval.

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
