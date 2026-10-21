# Asset Loop In architecture

Asset deposits and swaps run in the ordinary deposit and static Loop In FSMs.
They use the existing managers, swap registry, SQL stores, state history and
startup recovery. There is no second asset executor, request journal, recovery
worker, revision protocol or mirrored deposit lifecycle.

The earlier implementation added those layers unnecessarily. The existing
server already saves critical state before sending payment and releasing
reservations. Asset actions use that same approach. The generic FSM does not
need a persistence redesign to support this protocol.

## Ownership

| Data or operation | Owner |
| --- | --- |
| Asset deposit lifecycle and swap reservation | Ordinary `deposit.Deposit`, its state, ID and `SwapHash` |
| Asset proof, group, quantity and Bitcoin contract | Optional asset metadata on the deposit row |
| Signed deposit funding/refund package before publication | Deposit row, saved by the deposit FSM |
| Client swap lifecycle, accepted terms and complete fallback | Ordinary `StaticAddressLoopIn` and its optional asset data |
| Server secret, payment plan, claim authority and execution decision | Ordinary server `StaticAddressLoopIn` and its optional asset data |
| Bitcoin keys and ephemeral signing sessions | Existing LND wallet and signer services |
| Transfer publication, proof archive and proof reconstruction | Tapd through the SDK |
| Bitcoin deposit/HTLC policy | Loop-owned `swapkit` contracts |
| OP_TRUE asset commitments and transfer packages | SDK custom-anchor builder |

Native asset quantities remain separate from Bitcoin anchor satoshis. A client
swap initially has an unknown preimage, represented by SQL NULL. It never
stores a dummy secret. The migration preserves existing Bitcoin records and
known-preimage uniqueness on SQLite and PostgreSQL.

The ordinary FSM actions compose the SDK's Build, Commit and publication calls.
They sign through their existing LND clients and the shared
`staticaddr/script.CreateMuSig2Session` helper. `swapkit/tapsdk` retains the
Loop-specific contract and current-chain checks, plus proof recovery from an
offline peer's observed spend. Its operation contexts hold validated data for
one action; they own no services, persistence or lifecycle.

The deposit FSM saves a complete signed package before publication. Unsigned
and finalized transfers use the SDK's
`CustomAnchorTransferPackage` binary format directly, including its finalized
anchor PSBT. Finalization binds the proposed package to the approved committed
digest and verifies the complete witness. There is no second transaction or
recovery envelope.

Building actions inspect the negotiated contract and current chain, then rely
on the SDK builder to verify input proofs. They recheck chain membership before
committing. Validation-only actions explicitly verify proofs with the same
chain checks. A later action or restart validates again; no verification result
is cached across operations.

Native payment calls run in the existing FSM actions, using the ordinary LND
clients and tapd RPCs. Raw LND calls obtain authentication through
`RawClientWithMacAuth`; there is no additional connection or payment client.
Raw invoice and payment messages retain circuit IDs and expiry fields needed
for accepted-HTLC checks. Shared native-payment helpers in `assets` validate
native amount, group and circuit evidence using tapd's JSON metadata types.

The client stores the HOLD invoice in the normal `SwapInvoice` field and keeps
one finalized SDK package as its fallback and claim authority. The server
saves the generated native `SendPaymentRequest` with its fixed RFQ and
fee cap before dispatch. Recovery tracks the original hash before sending and
never obtains another quote for an exposed swap. Accepted circuit evidence
continues to bind settlement after the channel has closed.

Request IDs locate ordinary records when a caller repeats a completed request.
They do not create another request lifecycle.

## State and action mapping

| Existing phase | Asset behavior |
| --- | --- |
| Deposit creation | `PublishAssetDeposit` saves and publishes the signed funding package, then enters `Deposited` after proof verification. |
| Deposit `Deposited` | Uses normal reservation/expiry handling. Typed asset metadata excludes anchors from Bitcoin-only withdrawal and channel operations. |
| Deposit `PublishExpirySweep` / `WaitForExpirySweep` | Builds, saves and publishes the original CSV refund; verifies its asset proof before completing. |
| Client `InitHtlcTx` | Obtains the server-generated payment hash and creates the ordinary swap record. |
| Client `SignHtlcTx` | Creates a same-group HOLD invoice, prepares the payout, completes the fallback and verifies the actual incoming native HTLC. |
| Client `MonitorInvoiceAndHtlcTx` | Retries conditional claim submission from the saved fallback, obtains the server/on-chain preimage and observes local settlement. |
| Client `PaymentReceived` | Completes on verified local payment, without waiting for the server's on-chain claim. |
| Client timeout states | Publishes and verifies the unilateral asset refund using the saved fallback. |
| Server initialization / `PaySwapInvoice` | Validates the deposit, saves the offer and secret, and sends/tracks the native payout by its original hash. |
| Server `FinalizeHtlcTx` | Saves server signature exposure and accepts the client's completed claim authority. |
| Server `AwaitServerExecution` | Waits for trusted server policy; no client RPC can execute the swap. |
| Server publish / monitor / resolution states | Publishes the claim independently, tracks payout and proof outcomes, and retains reservations until resolution. |

An accepted HOLD payment is still unsettled. The server therefore prepares the
payment before claim signing, then decides execution and can claim even while
the client is offline. Those ordering differences require asset-specific
actions and recovery edges inside the existing FSM.

## Restart boundaries

The client saves the complete fallback and accepted native-payment evidence
before sending claim authority. The server saves signature exposure before
returning a partial signature, and saves its execution decision before making
the preimage available. Failed saves cannot authorize these effects.

Once authority may have escaped, recovery uses the original payment hash and
saved transaction packages. It never interprets a lost RPC reply as a failed
payment or unlocks an exposed claim on a timer alone. Client recovery uses
local invoice evidence, server status or the on-chain preimage. Server recovery
reconciles native payment and asset proof outcomes before releasing its lock.

Before commitment, interrupted initialization may abort and unlock through the
ordinary error path. The flow does not promise transparent retries of every
funding, offer or signing RPC. Incomplete MuSig2 sessions remain ephemeral;
there is no private signer database or persisted secret nonce.

The experimental schema and transaction encoding are not released formats.
This revision replaces earlier draft transaction/payment wrappers and duplicate
client fallback fields. Databases containing those draft formats must be
recreated. Restart recovery is tested for packages written by this revision,
not upgrades from earlier PR drafts.

## Validation and remaining work

The normal FSM/store tests cover recovery from saved state, native-payment
checks and saves before signature exposure. The server process suite runs the
shipping client/server binaries with their ordinary databases, including held
execution, publication restart, unilateral refunds and a closed asset channel.
The separate proof-import fixture covers a peer's already-mined transaction;
it does not implement another swap controller.

Fee escalation, real-node reorg coverage, dependency review and rollout work
remain in the [readiness document](asset-loop-in-readiness.md). They do not
require reinstating a parallel lifecycle or a general Loop durability framework.
