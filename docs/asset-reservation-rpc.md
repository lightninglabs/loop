# Asset reservation protocol

The local `looprpc.AssetReservations` service separates `Buy` (request a
quote) from `Approve` (permit payment under saved limits). `Get` and `List` support inspection and recovery without buying again.
Unapproved purchases expire automatically. Funded records accept canonical outpoints; pre-funding retries use a
stable ID. Local calls require the existing swap read or execute macaroon
permissions. Standalone loopd registers the service; it becomes usable with
experimental features and tapd enabled. Embedders must also register this
separate service to expose it.

Local `List.state` filters by one latest saved client state. The query reads
matching reservations and their latest state together, without loading full
histories. Alternatively, `List.active_only` excludes `QuoteRejected`,
`QuoteFailed`, `Canceled` and `Expired`, while retaining `Ready` and `NeedAdminAttention`.
The filters are mutually exclusive; omitting both returns all reservations. Names are case-sensitive,
and unknown names are rejected. Keep the same filter when following
`next_after_id`. For example:

```shell
loop asset reservation list --state Ready
loop asset reservation list --active_only
```

Startup excludes `QuoteRejected`, `Canceled` and `Expired` in the query.
It retains `Ready` for expiry monitoring and `NeedAdminAttention` for
administrative care.

The wire contract lives in `swapserverrpc/asset_reservation.proto`. It is
separate from Bitcoin Instant Out reservations. These definitions do not
enable a service; handlers and authentication wiring remain separate.

## Purchase and recovery

The client saves a random request ID, requested asset and amount, and its key
before `QuoteAssetReservation`. The server binds that ID to the authenticated
owner and request. An exact retry returns the same purchase and invoice,
including after expiry. It must never create a fresh bill or reprice it.
Changing the asset, amount, or key under the same ID is rejected. A fresh quote
requires a new purchase ID. Old unpaid quotes expire automatically.

Before saving a new purchase, the server checks funding capacity. A shortage
returns `OutOfRange` with `amount above current maximum`. The client saves
`QuoteRejected`, a terminal state visible through local `Get` and `List`;
`buy` reports the error and stops. No payment or server cancellation is needed.
Transient RPC errors remain retryable. An accepted, paid purchase still recovers
funding even when inventory later disappears.

The response may still be `QUOTING`. `GetAssetReservation` retrieves progress
and the immutable quote once available. The quote supplies both invoices, asset
terms, keys, Bitcoin equivalents, edge, expiry, and `prepay_rfq` plus
`probe_rfq`. These fields bind the invoices to their exact asset amounts and
route hints. The client validates both invoices, probes the full reserved asset
amount, then waits for approval. Settlement of the ordinary prepay is the only
signal that permits funding. The main Bitcoin equivalent is an estimate, not an
exchange-rate guarantee.

The local response reports `main_probe`, `main_probe_fee_msat`, and
`probe_fee_known`. The fee fields are available only when `probe_fee_known` is
true; a successful probe can lack fee data if recovery missed the live route.
`estimated_prepay_route_fee_msat` scales that fee by the prepay/main BTC amount
ratio, rounded up to a millisatoshi, without accounting for fixed hop fees.
Both fee estimates are unavailable unless the main probe succeeds. The prepay
invoice is validated and paid under the approved limits, but is not probed.

Normal approval requires a successful main probe. `Buy.skip_probe` skips the
automatic probe for a new purchase; it is saved before the worker starts and
survives a restart. Repeating `Buy` preserves the existing purchase's progress.
`Approve.skip_probe` separately records consent to buy when probing was skipped.
Skipping the probe never authorizes payment. There is one probe payment per
purchase. A failed probe cancels the unpaid purchase; another attempt requires
a fresh purchase ID, optionally using `Buy.skip_probe`. Recovery resumes an
unfinished probe or completes cancellation. It never replaces the saved quote
or revives a canceled purchase.
There is no public probe command or RPC.

The approval request accepts the displayed quote by hash, including its
asset fee and BTC prepay amount. The client checks the hash against the
immutable quote. It saves the prepay routing cap and `skip_probe` on the
reservation, in the same transaction as the transition to `PayPrepay`. That
state records consent. A retryable payment error after that write returns the
saved progress. If the approval reply is lost, the CLI queries the same ID;
if it cannot establish progress, it prints commands to inspect or resume that
purchase without creating another one. Recovery from `AwaitApproval` still waits
for the caller; recovery from `PayPrepay` resumes with the saved choices.

The quote identifies two Lightning nodes on the receiving side:

- `receiving_node_key` is the identity key of the server's receiving LND node.
  That node issues the prepay and probe invoices. It receives the asset prepay
  through its asset channel with the conversion peer.
- `edge_key` is the identity key of that conversion peer. It accepts the
  incoming BTC payment and forwards the corresponding asset payment to the
  receiving node at the rate agreed in the receiving RFQ.

The prepay follows this path:

```text
Client's LND -- BTC through Lightning --> conversion peer (edge_key)
             -- asset channel --> server's LND (receiving_node_key)
```

Both keys are 33-byte compressed Lightning node identity keys. The client
checks both invoice destinations against `receiving_node_key`, both route hints
against `edge_key`, and the prepay RFQ's peer against `edge_key`. The probe
invoice is a registered hold invoice for the full reserved asset amount. The
client attempts it through `SendPaymentV2`; the server verifies accepted asset
HTLCs and cancels it. It is never settled. The reservation's separate
`client_key` and `server_key` control the funded reservation output.

`AssetReservationStatus` summarizes progress reported by the server. It groups
internal server states into public statuses and does not define a shared state
machine. The client tracks its own progress and independently verifies delivery
before entering `Ready`, even when the server already reports `READY`.

`GetAssetReservation` and `ListAssetReservations` return public facts only.
They never expose private preimages, wallet locks, native funding requests,
or internal errors. Before funding, recovery uses the saved ID. After funding,
the CLI uses canonical `txid:vout`. IDs remain stable in storage. Binary IDs
must have exactly 32 bytes; keys must be valid compressed public keys.

`GetAssetReservationProof` returns the full exported proof for the exact
outpoint. The client verifies the asset, keys, script, amount, chain history,
required confirmations, unspent status, and remaining lifetime independently.
Server `READY` is not client `Ready`. `prepay_credit` is the exact asset fee
after verified settlement, never a conversion inferred from a BTC total.

`CancelAssetReservation` requests cancellation; its reply may still say
`CANCELING`. Retry or poll after a lost reply. Only an authoritative canceled
invoice and resolved client payment establish an unpaid outcome. If settlement
won the race, continue delivery. No refund RPC is provided.

## Boundaries

All methods require L402 ownership checks. Unknown IDs, another owner's IDs,
and internal lookup failures return the same opaque status and message.
Detailed causes stay in server logs. Validate canonical outpoints before
lookup or dispatch. Page size is bounded; list results contain only owned
records. This version uses RPC calls and periodic status checks, without
notification-stream integration. Later notifications can prompt fresh reads.

The local client API exposes buy, approve, get, and list.
The approval request includes the displayed quote hash and prepay routing
cap. `skip_probe` bypasses only the requirement for a successful main probe.
Approval never reaches the server as a "paid" message. The CLI buy command runs
quote, probe, approval, payment, and verification in sequence; the daemon keeps
recovery running after the CLI disconnects. Standalone loopd registers the local service.

## Full-amount hold probe

The client verifies the probe hash against `ProbeHash(reservation_id)` and its
native RFQ against the reserved amount. Quote contents remain immutable.
`GetAssetReservation` reports `probe_succeeded` (a saved exact asset receipt)
and `probe_canceled` separately. A failed Lightning payment by itself is not
probe success. Both remote cancellation and local payment resolution are
required before the client completes probing.

`CancelAssetReservation` accepts `probe_only` to cancel only the probe invoice,
in its dedicated `CancelAssetReservationRequest`, when probing is skipped or
a successful probe needs its hold released. Failed probes cancel the entire
unpaid purchase. The request embeds an `AssetReservationSelector` and keeps
the cancellation option separate from reservation identity. Cancellation is
idempotent and never recreates an invoice. A failed probe requires a fresh
purchase ID to try again. A successful probe also ends in invoice cancellation,
but leaves the purchase available for approval.

Quote acquisition has a one-minute deadline measured from persisted purchase
creation. An invalid quote or an exhausted deadline ends in `QuoteFailed`
without payment. Transient server errors may retry within that budget; a
restart does not extend it. The CLI reports that the quote was unavailable or
invalid and no payment was sent. Server-side unpaid invoices expire separately.

Before approval the CLI prints the exact BTC prepay in sats and millisatoshis,
and the implied prepay and probe prices in sats per asset unit. RFQ rates must
be within 5% of each other, relative to the lower rate. Both rates still come
from the server; consistency alone cannot establish a fair market price.

Reservation recovery runs asynchronously. A bad row or failed initial read
makes this feature unavailable and logs the cause without stopping ordinary
Loop In or Out. Repair the underlying issue and restart for recovery. Ready
reservations verify their saved proof once per process and subsequently refresh
chain state; an idle bounded chain watch does not log an error.
