# Asset reservation protocol

The local `looprpc.AssetReservations` service separates `Buy` (request a
quote) from `Approve` (permit payment under saved limits). `Get`, `List`,
`RetryProbes`, and `Cancel` support inspection and recovery without buying
again. Funded records accept canonical outpoints; pre-funding retries use a
stable ID. Local calls require the existing swap read or execute macaroon
permissions. Standalone loopd registers the service; it becomes usable with
experimental features and tapd enabled. Embedders must also register this
separate service to expose it.

The wire contract lives in `swapserverrpc/asset_reservation.proto`. It is
separate from Bitcoin Instant Out reservations. These definitions do not
enable a service; handlers and authentication wiring remain separate.

## Purchase and recovery

The client saves a random request ID, requested asset and amount, and its key
before `QuoteAssetReservation`. The server binds that ID to the authenticated
owner and request. An exact retry returns the same purchase and invoice,
including after expiry. It must never create a fresh bill or reprice it.
Changing the asset, amount, or key under the same ID is rejected. A fresh quote
requires a new request after cancellation of the old unpaid purchase.

The response may still be `QUOTING`. `GetAssetReservation` retrieves progress
and the immutable quote once available. The quote supplies both invoices,
asset terms, keys, Bitcoin equivalents, edge, expiry, and `prepay_rfq`. That
field carries the native receiving RFQ, without secrets, so the client can
bind the prepay invoice to its exact asset fee and route hint. The client
validates both invoices, probes only the estimated main amount, then waits
for approval. Paying the hold prepay is the only signal that permits funding.
The main Bitcoin equivalent is an estimate, not an exchange-rate guarantee.

The local response reports `main_probe` and `main_probe_fee_msat`.
`estimated_prepay_route_fee_msat` scales that fee by the prepay/main BTC amount
ratio, rounded up to a millisatoshi, without accounting for fixed hop fees.
Both fee estimates are unavailable unless the main probe succeeds. The prepay
invoice is validated and paid under the approved limits, but is not probed.

Normal approval requires a successful main probe. `Buy.skip_probe` skips the
automatic probe for a new purchase; it is saved before the worker starts and
survives a restart. Repeating `Buy` preserves the existing purchase's progress.
`Approve.skip_probe` separately records consent to buy without a successful
probe, including after a failed attempt. Skipping the probe never authorizes
payment. An explicit `RetryProbes` clears the skip preference and runs the
main probe for the same quote.

The approval request accepts the displayed quote by hash, including its
asset fee and BTC prepay amount. The client checks the hash against the
immutable quote. It saves the prepay routing cap and `skip_probe` on the
reservation, in the same transaction as the transition to `PayPrepay`. That
state records consent. Recovery from `AwaitApproval` still waits for the
caller; recovery from `PayPrepay` resumes with the saved choices.

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
checks both invoice destinations against `receiving_node_key`, both route
hints against `edge_key`, and the prepay RFQ's peer against `edge_key`. The
probe invoice estimates a later payment through the same edge; it is never
paid. The reservation's separate `client_key` and `server_key` control the
funded reservation output.

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

The local client API will expose quote, approve, cancel, get, and list.
The approval request includes the displayed quote hash and prepay routing
cap. `skip_probe` bypasses only the requirement for a successful main probe.
Approval never reaches the server as a "paid" message. The CLI buy command runs
quote, probe, approval, payment, and verification in sequence; the daemon keeps
recovery running after the CLI disconnects. Local RPC handlers and CLI wiring
remain a separate step.
