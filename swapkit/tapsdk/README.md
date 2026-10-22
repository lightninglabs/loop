# Native SDK policy adapters

`swapkit` owns Bitcoin contracts. This package adapts them to the native Taproot
Assets SDK and checks Loop's independently agreed terms. The ordinary caller's
FSM owns decisions, persistence, key locators, ephemeral LND sessions, signing,
proof import and publication. These helpers do not introduce another manager or
transaction engine, and this toolkit change does not install Sputnik's complete
production Loop In FSM.

The dependency foundation is SDK v0.3.0 with the exact proof-export locator and
received-key metadata fixes from SDK #187/#188. `go.mod` pins source
commit `ca09087d03513069ff06cab99c56ddc50ddc0914`, which also adapts activation
checks to the pinned Taproot Assets #2366 API. The remaining dependency PRs are
pinned through immutable replacements; no local SDK preparation script is
required.

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
   Both enforce bounded genesis provenance and the live tapd v0.8.6 floor.
3. `PrepareInputs` restores exact confirmed proof lineage and declares public
   asset witnesses. Call the native `wallet.NewCustomAnchorTxBuilder().Build`.
   Native SDK Build verifies lineage and constructs allocations and commitments;
   do not repeat generic proof verification before Build.
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
Generic callers can use the native publication and proof APIs directly.
`BindFinalPackage` accepts a peer's final Bitcoin witnesses only for the saved
approved native package. `DecodePackage` rejects packages over 128 MiB before
JSON decoding and validates native consistency and signing policy. Saved-package
validation uses the committed PSBT because finalization can remove its sighash
metadata. Recovery of an observed refund preserves its mined version, locktime
and sequence, including older non-RBF refunds. A digest does not itself cover final witnesses: those
are independently validated by the native SDK.

## Validation

Run `go test ./swapkit/... ./internal/assetproof ./internal/assetpolicy` with the
pinned Go toolchain. The tests cover native Build/Commit and package encoding,
legacy script vectors, exact policy substitution, signed compact parents,
confirmation handoff and native publication boundaries. The tagged SDK itests
require their configured regtest services; Nautilus also exercises consuming
contracts through its normal asset integration test harness.
