# Swapkit SDK integration

This adapter uses the released `github.com/lightninglabs/tap-sdk` v0.1.1.
That release provides the v0.3.0 API on the pre-btcd-v2 dependency graph.
Loop does not maintain a copy of the SDK or depend on the migration PRs.

`swapkit` owns Bitcoin contracts. This package adapts them to the native Taproot
Assets SDK and checks Loop's independently agreed terms. The ordinary caller's
FSM owns decisions, persistence, key locators, ephemeral LND sessions, signing,
proof import and publication. These helpers do not introduce another manager or
transaction engine, and this toolkit change does not install Sputnik's complete
production Loop In FSM.

The release includes the exact proof-export locator and received-key metadata
fixes needed by these flows. Loop pins Taproot Assets v0.8.9 and the bounded
legacy TLV decoder from the same tag. Consumers must
copy the TLV replacement from Loop's `go.mod`, since Go does not inherit it.

For an existing authenticated tapd connection, construct the wallet with
`sdk.NewWallet(tapsdk.NewClientFromConn(conn, timeout), network)`. This
adapter composes the released SDK RPC clients, preserves custom-anchor and
grouped-registration capabilities, and removes the SDK's empty macaroon
header so existing credentials remain authoritative. The caller
owns credentials and connection shutdown; closing this wallet does not close
the borrowed connection. Consumers do not need a separate SDK client wrapper.

## Asset and Bitcoin contracts

`LegacyOPTrueOutput` preserves the existing fixed NUMS internal key and single
`OP_TRUE` asset leaf using native External mode. `LegacyOPTrueDescriptor` returns
its exact key derivation and witness. New deposit, reservation and fallback
callers should select this policy. Contract changes never implicitly migrate
already funded asset script keys.

`OPTrueOutput` retains Sputnik's explicit supplied-key SDK tree constructor;
`SDKOPTrueOutput` is its clearly named alias. They require deliberate selection.
`OPTrueSpendInfo` validates saved native SDK witnesses and reconstructs only the
exact legacy External tree when a saved output has no native OP_TRUE descriptor.
Ordinary External wallet outputs cannot be promoted to public witnesses. Native
External package summaries remain unchanged and correctly have a nil
`OPTrueSpend` field.

`AnchorPlan`, `CooperativeSigningPlan` and `ScriptSigningPlan` accept the pure
core's contracts: CSV deposit, CLTV reservation, CSV HTLC and Sputnik's CLTV HTLC.
`InputBinding` records exact issuance, optional full group key, quantity, script
key, outpoint and optional Bitcoin contract/spend terms. It has no client/server
role assumptions, so the server-funded reservation and client-funded deposit use
the same policy boundary. The older `DepositContext`/`SwapContext` helpers retain
Sputnik's specific Loop In negotiation semantics.

## Native construction and approval

Use the following order for a confirmed-input transaction:

1. Retain independent input/output terms and the Bitcoin template. Obtain every
   funding `WitnessUtxo` from an authenticated Bitcoin backend; a peer's PSBT is
   not its source of authority. `Chain.Unspent` includes mempool spends. Use `CheckFundingInputs` with
   `PrevoutSource` to authenticate every value and script against the current
   Bitcoin backend; `LNDChain.PrevOutput` supplies this optional full lookup.
   Confirmation checks verify the proof's transaction Merkle path against a
   fresh block hash, without downloading full blocks. `PrevOutput` can also
   back `Unspent` when no separate callback is supplied. Callbacks
   must use the current mempool-inclusive view; availability is never cached
   across operations.
2. `InspectInputProof` checks one isolated input. `InspectInputProofs` checks a
   complete batch of active and passive bindings and their union commitment.
   Both enforce bounded genesis provenance and the live tapd v0.8.9 floor.
3. `PrepareInputs` restores exact confirmed proof lineage and declares public
   asset witnesses. A caller-provided witness is verified to commit to its input
   script key before it is declared, so a peer cannot bind a wallet script key
   to an unrelated internal key. Call the native
   `wallet.NewCustomAnchorTxBuilder().Build`. Native SDK Build verifies lineage
   and constructs allocations and commitments; do not repeat generic proof
   verification before Build.
4. Call `ApprovePlan` with the independently retained Bitcoin template, concrete
   `OutputBinding` allocations and absolute fee cap. It checks every issuance,
   allocation, asset key derivation, Bitcoin contract and complete transaction
   terms using native commitment previews. Group allocations expanded into
   several issuances need a binding for each concrete issuance.
5. Immediately before direct native `plan.Commit`, repeat `CheckNodeVersion`,
   `CheckFundingInputs` on the native plan PSBT, and `CheckInputsCurrent`. The latter checks complete active/passive inventory,
   current chain inclusion, confirmation depth and every funding outpoint.
6. Call `CheckSigningPolicy` on the committed package before opening signing
   sessions or signing through native SDK requests. Every declared input must
   use SIGHASH_DEFAULT; wallet-managed fee inputs retain wallet policy. The
   encoded package must fit the 128 MiB recovery bound before releasing authority.
7. Persist the native committed package and its independently approved
   `CommittedPackageDigest` before exposing signatures. Use normal LND signing
   and the SDK's signing requests/final-witness APIs. Persist completion before
   external effects.

For native WalletFunded mode, set `MaxFeeSat` and the deterministic lock ID from
independent caller policy. `ApproveWalletFundedPlan` checks the fixed template
and allocations before funding, when the final fee/change cannot yet be known.
Use `AnchorChangeOutputAdd` for newly generated wallet change. Existing-output
and NoNew change policies are rejected so funding cannot change fixed recipients.
`CheckFundedPackage` validates the returned native funding summary against the
independent cap before anchor signing, release or publication. Exact/P2A plans
use `ApprovePlan`, which also verifies the complete actual fee, including a zero
fee when explicitly approved.

Validation-only acceptance uses `VerifyInputProof` or Sputnik's `VerifyDeposit`/
outcome helpers instead. They bind authenticated SDK verification results to
the exact local proof and bracket the verification RPC with current-chain
checks. Verification reuse is confined to one operation. Never carry validation
across actions, retries or restarts. Local saved-package decoding and Bitcoin
witness recovery do not depend on the live node version.

Known co-anchored active/passive assets remain supported by the batch inventory
boundary and native SDK passive policy. Omitting an asset fails closed. The
single-input Loop In contexts and SDK compact paths require isolated anchors.

## Group evidence and issuance metadata

`VerifyGroupMembership` authenticates an anchor-first set of genesis proofs
against an independently supplied group and required issuance IDs. Each decoded
proof's asset is validated before it is reused, so malformed evidence is
rejected rather than panicking while it is re-encoded. The caller sets the
aggregate proof-byte limit and decides which issuances its quote needs.
`SameGroupKey` uses the SDK's x-only identity comparison after validating both
compressed keys. `IsGroupMember` checks declared fungible group identity only;
it is not proof verification.

`VerifyProofWithIssuance` inserts the verified genesis proof into the local
universe before asking tapd to verify the full history. It does not import a
wallet asset or register a received transfer. The caller must still enforce
its message limit and bind the result to the expected contract and output.
These helpers accept authenticated tapd RPC clients and preserve node errors.

## Presigned descendants and publication

`NewApprovedParents` implements the SDK's native confirmed-proof and unconfirmed
anchor verification interfaces for one operation. It accepts exact saved parent
committed digests, validates complete Bitcoin witnesses and checks the signed
transaction graph and current root funding. Install it with
`builder.SetConfirmedProofVerifier(parents)`, call native Build, then repeat
`parents.CheckPlan(ctx, plan)` and the independent output/Bitcoin approval before
Commit. `CheckPlan` binds the exact native request snapshot and repeats current
root-funding checks; a verifier must not be reused for a substituted request.
The chain passed to compact-parent and confirmation-handoff helpers must also
implement `PrevoutSource`: external Bitcoin fee and asset inputs require full
authenticated value/script comparisons, as well as mempool-inclusive unspent
checks. Known parent edges use the exact independently approved signed outputs.

The SDK owns compact asset transition validation. Do not replace this adapter
with an attestation that only checks a parent transaction ID.

Native Publish rejects a child that still contains unconfirmed compact steps.
After the parents confirm, `ConfirmPackageInputs` requires the same authenticated
prevout source and materializes native confirmed histories, verifies their exact
bindings/current chain and updates only proof
metadata. It preserves all asset state, Bitcoin commitments and final
witnesses. The resulting native package has a new committed digest: persist the
old-to-new digest association and updated package first, then import every
returned native `ProofFile` through `wallet.ImportProofFile`, and finally call
`wallet.PublishCustomAnchorTransfer`. Import/publication ambiguity and retry
remain owned by the FSM.

`LogAndExport` retains Sputnik's single-input/output publication/recovery
helper. It registers exact descriptors, uses exact issuance/script/outpoint
proof locators, and logs already observed Bitcoin spends without rebroadcast.
tapd records every publication request as a new transfer, so a retry never
logs again: an archived exact proof returns its own confirmation result, and an
anchor already in tapd's transfer log returns `ErrPending` until its proof is
archived. Call it again to poll; serialize invocations for one package, since
the lookup and the publication are not atomic.
Generic callers can use the native publication and proof APIs directly.
`BindFinalPackage` accepts a peer's final Bitcoin witnesses only for the saved
approved native package. `DecodePackage` rejects packages over 128 MiB before
JSON decoding and validates native consistency and signing policy. Saved-package
validation uses the committed PSBT because finalization can remove its sighash
metadata. Recovery of an observed refund preserves its mined version, locktime
and sequence, including older non-RBF refunds. A digest does not itself cover final witnesses: those
are independently validated by the native SDK.

`NewSwap` validates a peer's fallback package against the authenticated deposit
before it is ever co-signed. It binds the committed PSBT's signed previous
output to the deposit output, rejects a committed asset that carries its own
lock time, and requires the zero-fee pay-to-anchor form described below. The observed
refund and claim discovery helpers tolerate a `Chain` that returns a block
without the watched spend, recognize the agreed leaf when the spend carries a
BIP-341 annex, and reject an annex spend from reconstruction rather than
mis-slicing its witness. A reorg observed while verifying an outcome is
reported as retryable, not invalid.

## HTLC transfers and fee bumping

The cooperative transaction that moves a funded contract output into an HTLC is
signed when the swap is set up and may be published much later, so it does not
fix its own fee. `HTLCRequest` builds it for either direction from `HTLCTerms`:
the funding contract (a deposit for Loop In, a reservation for Loop Out), the
verified funded output and its proof, the HTLC contract and the public MuSig2
session context. The result is a version 3 transaction with one input and two
outputs: the HTLC at index 0, which keeps the whole funding anchor value, and a
zero-value pay-to-anchor (P2A) output at index 1. The transaction pays no fee.

`ValidateHTLCPackage` is the check a party runs on a peer's package before
co-signing. Build `HTLCTerms` only from values the party agreed to or verified
itself: its own keys, payment hash and expiry, and the funded output it checked
with `VerifyInputProof` or `VerifyDeposit`. The validator accepts only the exact
transaction `HTLCRequest` builds for those terms. It rejects another payment
hash, expiry, key role or refund policy, another funding contract, extra inputs
or outputs, a transaction that pays its own fee, and an asset that carries its
own lock time. It does not verify proof history, chain state or timelock
ordering; use the proof helpers and `swapkit.LoopInTimelocks` or
`swapkit.LoopOutTimelocks` for those.

`FallbackRequest` and `NewSwap` are the Loop In forms of the same two calls,
with the terms taken from the offer and the deposit contract.

A swap that settles off chain ends without the HTLC transaction: both parties
sign a direct spend of the funded output to the party that would have claimed
the HTLC. `ValidateCooperativeSpend` is the funder's check of that package
before it co-signs. By then the funder is paid, so the outputs are not
constrained. The check only ensures that the signature cannot be used for
anything else: the funded output must be the single input, with no further
asset, passive or wallet input, the signed transaction must commit to the
authenticated funding output's value and script, and the signing plan must be
the funding contract's cooperative key path under the agreed session context.
It does not decide whether the swap is settled; that stays with the caller.

The publisher spends the P2A output in a child transaction funded from its own
wallet and submits parent and child as one package, for example through lnd's
`walletrpc.SubmitPackage` (lnd v0.21.2 or later with a bitcoind backend). The
child sets the fee for both and can be replaced with a higher-fee child without
a new signature from the peer. Anyone can spend a P2A output, so the peer can
attach a child too; version 3 relay rules let a higher-fee child replace it.

The SDK seals such a package for external broadcast, so tapd only logs it.
`FallbackProof` and `LogAndExport` therefore never broadcast a fallback: submit
the package first, then call them to log the transfer and export its proof.
`maxSweepFee` in `FallbackRequest` is the fee the HTLC output must still be
able to pay for its later claim or refund, which remain single-input,
single-output transactions.

## Validation

Run `go test ./swapkit/... ./internal/assetproof ./internal/assetpolicy` with the
pinned Go toolchain. The tests cover native Build/Commit and package encoding,
legacy script vectors, exact policy substitution, signed compact parents,
confirmation handoff and native publication boundaries. Run daemon-backed
asset lifecycle cases in the Loop server integration harness.
