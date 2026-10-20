# Asset static loop-in client execution

This package implements the durable client half of the server-preimage
protocol. The server creates the payment hash and decides when to disclose its
preimage. The client creates a same-group asset HOLD invoice, verifies its
incoming committed HTLC locally, and keeps a fully signed SDK fallback before
sending any signature to the server.

The controller dispatches events through the same `loopin/flow` transition
policy used by the existing Bitcoin FSM, selecting its `ServerAsset` branch.
The branch retains preparation and exposure across recovery; Bitcoin's branch
keeps its existing unlock transitions. Mandatory journal writes remain inside
the controller, before effects, because the legacy FSM entry hook cannot fail
closed on persistence errors. `InitHtlcTx` journals the offer, shared deposit
reservation, and invoice intent/result. `SignHtlcTx` receives and persists the
complete verified recovery bundle. `MonitorInvoiceAndHtlcTx` begins only after
local acceptance is verified and signature exposure is durably recorded.
`PaymentReceived` requires local invoice settlement. `Succeeded` additionally
requires a confirmed asset claim. The timeout branch uses `SweepHtlcTimeout`
and `HtlcTimeoutSwept`, retaining locks until a confirmed refund includes
spendable asset proofs.

The shared `deposit/flow` graph also governs each selected input's lifecycle.
`DepositState` advances with the same atomic journal write as exposure or
confirmed claim/refund evidence. `LoopingIn` becomes `AssetClaimExposed` before
signatures can escape, then `SweepHtlcTimeout` during refund recovery. Only
confirmed outcomes reach `LoopedIn` or `HtlcTimeoutSwept`. A cancellation
before exposure returns to `Deposited`. Generic errors and expiry cannot
release a protected claim or invoke a Bitcoin-only sweep. Older journals
infer their deposit state from the existing evidence rather than losing locks.

The existing `loopin.SqlStore` implements this package's `Store` interface.
Revision checks and deposit state updates use the same SQL transaction as
execution checkpoints. Terminal cleanup atomically releases only the matching
request and outpoint; no second journal or receipt bucket is required.
Terminal records stay active until that transaction completes.

Persisted JSON field names are part of the recovery format. Explicit tags
preserve the original exported names, including their capitalization. Renaming
those fields requires a migration that also preserves request and signing
bindings; a formatting change must not rewrite journal identities.

The caller persists invoice intent with `Start`, obtains the fixed invoice
with `PrepareInvoice`, submits it through the authenticated server transport,
and passes the server's fallback to `ReceiveFallback`. The SDK adapter must
independently validate all proofs, packet outputs, recovery fee tiers, signing
requests, and server signatures, then return a fully publishable bundle.
`ExposeClaim` calls local `assetln.Receiver.Prepared` through its adapter,
persists that evidence and exposure, and only then sends the exact claim
signatures. Every retransmission rechecks the same live local circuit and
admission deadline. After evidence collection and again after the journal
flush, it rechecks wall-clock expiry, context cancellation and the current
chain height plus the execution margin. An old exposure marker never permits
sending signatures into an expired payout. An uncertain response never clears exposure.

Use `ReceivePreimage` for authenticated server notifications or a preimage
recovered from a verified claim witness. It verifies the hash and journals the
preimage before settling the HOLD invoice. `CooperativeSignature` rechecks
local settlement and fixes its request and resulting signature durably. A
server's claimed payment state cannot authorize an unconditional cooperative
spend. `Recover` resumes the persisted boundary after restart; adapters must
recover existing invoices, RFQs, signing sessions, and broadcasts by the same
hash, without replacing them.

After signatures may have escaped, an elapsed refund height or a mempool
transaction cannot authorize cancellation or deposit release. The transaction
adapter must establish the confirmed outcome of every committed deposit and
validate the resulting spendable asset proofs. Local Lightning and chain
observations are authoritative; server availability must not be required for
unilateral recovery. `Recover` first tries a preimage from the authenticated
local settled invoice, server disclosure, or chain witness. If no settlement
can complete and either the offer expires or the refund height is reached, it
journals cancellation and calls `RecoverRefund`. That idempotent adapter
publishes and monitors the persisted SDK fallback and refund as their timelocks
permit. Every retry verifies the current confirmed proofs again, including
after a reorg. A locally settled invoice or unavailable local settlement check
prevents publishing a refund; an unavailable server does not. Preimage
discovery gets at most one quarter of the recovery attempt's remaining budget,
capped at one second, leaving the parent context for local settlement checks
and refund publication. Within discovery, the remote server GET receives at
most one quarter of the remaining discovery budget, capped at 250 milliseconds.
Timeouts and malformed peer preimages still fall through to the independent
chain witness lookup.

The journal contains payment preimages and signed recovery material and must
be protected and backed up like the client wallet. Private MuSig2 nonces stay
inside LND's ephemeral sessions. The controller and normal Loop SQL store
provide durable execution. The normal `loopd` assembly uses its existing Loop
In manager and the `swapkit/tapsdk` adapter with normal LND signing services. The regtest integration harness also exercises these
boundaries with SDK transactions and local asset Lightning nodes.

The `assetclient.Session` adapter also resumes the network handshake: a restart
in `InitHtlcTx` or `SignHtlcTx` retries the original invoice preparation and
fallback retrieval, including lost responses. Expired offers that have not
exposed signatures are canceled locally. Offer admission binds the server
response to the deterministic request digest, the exact selected outpoint set,
the original contract bytes, native fee/debit limits, and the full compressed
group key including parity.

`Session.RecoverActive` performs one bounded recovery batch with independent
action deadlines. The existing Loop In manager schedules these batches beside
pending request recovery and Bitcoin notifications. It cancels and joins all
asset work before shutdown closes the shared store or node connections.
Every action must honor context cancellation.

The normal [`loopd` assembly](../assets.md) restores asset proof inventory and
shared Bitcoin deposit exclusion before starting recovery. Creation identities
are persisted before RPC calls. The normal SQL record retains the fixed
unsigned package and completed fallback across process restarts. An unfinished
signing round uses fresh nonces; it never restores private signer state. The same manager also recovers
original CSV deposits that never reached a swap controller.

See the [asset Loop In transitions](../asset-fsm.md) and
[asset deposit transitions](../../deposit/asset-fsm.md) for the shared graphs.
