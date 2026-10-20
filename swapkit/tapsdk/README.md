# Loop asset deposits and HTLCs

Loop owns `DepositKit` and `SwapKit` in
`github.com/lightninglabs/loop/swapkit/tapsdk`. They put the spending policy on
the Bitcoin anchor while using an `OP_TRUE` asset leaf. Both produce `CustomAssetOutput` values for
`Wallet.NewCustomAnchorTxBuilder()`. Applications use SDK keys, asset references,
amounts, hashes, and witness bytes; they do not need `taprpc`, virtual PSBT, or
Taproot Assets commitment types to construct the contract.

The kits cover contract construction and Bitcoin witness assembly. The existing
custom-anchor builder owns asset selection, proof verification, virtual
transactions, commitment construction, signing requests, and proof delivery.
Applications still own invoices, exchange rates, deposit reservations, signing
sessions, persistence, and the decision to release a preimage.

## Bitcoin policies

| Contract | Cooperative path | Unilateral paths |
| --- | --- | --- |
| `DepositKit` | Client and server MuSig2 | Client signature after a block-based CSV delay |
| `SwapHTLCLegacyCSV` | Sender and receiver MuSig2 | Receiver signature plus preimage after one block; sender signature after the configured CSV delay |
| `SwapHTLCStaticLoopInV3` | Sender and receiver MuSig2 | Receiver signature plus preimage after one block; sender signature after the configured CLTV height |

Policy zero is invalid. CSV delays must be 1–65535 blocks; CLTV expiry must be a
positive block height below 500,000,000. Time-based expiries and BIP68 disable
flags are rejected. Participants must have distinct x-only keys, including when
their compressed encodings have opposite parity. HTLC script keys and MuSig2
internal keys are supplied separately, as in Loop's V3 HTLC.

The deposit refund script is byte-compatible with Loop's static address script.
Both HTLC policies require the receiver's signature and a 32-byte preimage,
followed by a one-block CSV restriction. The script applies `OP_HASH160` to the
preimage and compares it with `RIPEMD160(payment_hash)`, where the payment hash
is `SHA256(preimage)`. The legacy refund uses CSV; V3 uses CLTV. Fixed vectors
also cover the MuSig2 key, complete Bitcoin control blocks, and anchor script.

The asset commitment is an additional sibling of the Bitcoin policy subtree.
The kit's spend helpers take `TaprootAssetRoot` from the verified builder output,
append it to the policy leaf's inclusion proof, and calculate the final parity
bit. The resulting `PkScript` must match the authenticated spent Bitcoin output
before a signer authorizes a transaction.

## Building and spending outputs

1. Construct a kit with the negotiated immutable keys, policy, and expiry.
2. Call `Output` with an SDK `AssetRef`, native `uint64` amount, Bitcoin anchor
   index/value, and a unique asset internal key. Use a group-key reference for
   fungible assets. The helper rejects amounts that cannot fit the underlying
   signed transaction fields.
3. Build and commit the normal custom-anchor request. Persist the complete
   transfer package and each concrete output's `OPTrueSpend` data.
4. When spending, supply the confirmed proof file or verified proof path and
   `CustomAssetWitnessCallerProvided`, using the exact output's
   `OPTrueSpend.WitnessStack()` as the asset witness.
5. For a cooperative spend, use `CooperativeSigningPlan`. It supplies the
   participants in the sorted order used for key aggregation. Sign the SDK's
   digest-bound MuSig2 request with its complete `TaprootMerkleRoot` tweak.
6. For a unilateral spend, use `SuccessSpend` or `TimeoutSpend` and copy the
   returned leaf and control block into the input PSBT. Apply its `Sequence`
   and `LockTime` before building or signing. Add `SigningPlan(inputIndex)` to
   the custom-anchor request.
7. The witness helpers return a complete Bitcoin witness. When calling
   `ApplyScriptPathWitness`, pass only its unlocking prefix (preimage and
   signature, or signature alone). The SDK supplies the committed script and
   control block, and verifies the witness against the actual transaction.
8. Publish through the custom-anchor lifecycle and confirm/import the resulting
   asset proof as usual. Script execution checks transaction timelock fields;
   the application and chain also enforce actual maturity.

Before committing a spend, import every selected confirmed input into the same
tapd instance that will publish and log the transfer. Verification and export
alone do not establish this archive precondition: tapd's confirmation handler
rebuilds successor proofs from its durable input records. An external `OP_TRUE`
script key must first be registered with `DeclareScriptKey`, using its complete
script key, `OPTrueSpend.InternalKey`, and the asset script-tree root reconstructed
from `OPTrueSpend.ControlBlock` and `LeafScript` as the tweak. Then use
`ImportProofFile` under the application's authenticated universe/import policy.
Importing marks that external script as locally tracked; it does not give tapd
the Bitcoin keys or change the cooperative spending policy. Retain the selected
proof and metadata for recovery. See the
[custom-anchor proof precondition](https://github.com/lightninglabs/tap-sdk/blob/main/docs/design/advanced-custom-anchor-transactions.md#asset-identity-and-selection).

Fresh asset internal keys prevent duplicate output commitment keys. The builder
also derives an issuance-specific nonspendable sibling next to `OP_TRUE`, so
different tranches of a fungible group have different script keys and witnesses.
Do not regenerate a universal asset witness or use one tranche's witness for
another. No timelocks or signatures are added to the `OP_TRUE` asset leaf.

## Server execution authority

An `OP_TRUE` asset leaf does not give the server unilateral control of the
Bitcoin deposit. Before its refund delay, both Bitcoin key holders must sign a
deposit spend. For a server-controlled swap protocol, bind the negotiated
payment hash to a server-held preimage. The server prepares the asset payout
against the client's HOLD invoice; the client independently observes its local
accepted asset HTLC and persists the fully signed fallback before exposing its
claim signatures. The server then decides when to release the preimage. Neither
kit changes Lightning invoice ownership or enforces application settlement
ordering.

An ownership proof for an `OP_TRUE` asset proves no exclusive ownership. It
must never substitute for the complete issuance/transfer proof history or an
application's authenticated deposit assignment. Use the custom-anchor proof
verification flow and verify the expected asset, amount, outpoint, and Bitcoin
policy before signing. A root hash by itself is not provenance evidence.

## Compatibility and validation

The legacy policy preserves the old Bitcoin contract; new builder outputs use
the SDK's unique asset script keys. It does not reinterpret already-funded
outputs or migrate their persisted witnesses. An application spending an old
output must supply that output's exact proof and asset witness.

The Bitcoin contract implementation lives in the independently versioned
`github.com/lightninglabs/loop/swapkit` module. It requires Go 1.25 and split
btcd v2 script primitives, with no lnd, tapd, or parent SDK dependency. Its
byte-oriented API can coexist with older wallet dependencies. The Loop server's
legacy kits delegate scripts, aggregation, control blocks, and witnesses to
this shared implementation while retaining their proof/packet adapters.

This Loop adapter and the generic SDK builder require Go 1.26 and the
newer lnd/tapd graph. Importing the standalone contracts does not migrate an
application's complete proof and transaction-building workflow to that builder.

Unit tests cover fixed contract vectors, Bitcoin VM success and failure paths,
timelock boundaries, key and amount validation, signature encoding, defensive
copies, and agreement with custom-anchor commitment previews. The Loop-owned `itest/TestSwapKitDepositLifecycle` integration test exercises
the SDK lifecycle against
tapd, including cooperative deposit spending and unilateral HTLC settlement.
It does not exercise a Loop client/server Lightning swap or pricing policy.

## Runtime responsibilities

The SDK already builds virtual transactions and commitments, checks proofs and
sealed transfer packages, assembles Bitcoin witnesses, and publishes transfers.
The local runtime supplies Loop's contract and recipient checks, durable wallet
effects, current-chain and mempool checks, and recovery when a peer disappears.
Original deposit refunds and HTLC outcomes share one observed-spend proof
reconstructor and one recipient/Bitcoin-policy verifier. The wallet signer keeps
nonce bindings across process restarts; the SDK supplies signing requests and
signature verification, but does not persist LND signing sessions.

The remaining confirmed-proof assembler is a compatibility boundary:
`AssetProofPath.ConfirmProofFile` requires v1 assets when the path has transition
steps, whereas deposits may carry a historical v0 base. Loop retains that
assembler and verifies the completed lineage through tapd. Replacing it must
preserve the v0 regression test and the current-chain checks; importing a proof
alone does not establish current confirmation or unspentness.

## Development setup

The adapter is a separate Go module. Until SDK PRs
[#187](https://github.com/lightninglabs/tap-sdk/pull/187) and
[#188](https://github.com/lightninglabs/tap-sdk/pull/188) are released, its
`go.mod` expects a sibling `tap-sdk` checkout next to the Loop repository.
From a fresh Loop checkout, prepare the pinned SDK with:

```sh
git clone https://github.com/lightninglabs/tap-sdk.git ../tap-sdk
git -C ../tap-sdk checkout 8521da12bb7c01b5ed01e80e0443977df07fb2b9
git -C ../tap-sdk apply "$PWD/swapkit/tapsdk/dev/tap-sdk.patch"
(cd swapkit && go test ./...)
(cd swapkit/tapsdk && go test ./...)
```

The patch contains the two generic SDK fixes and their regression tests, not
Loop contracts. If the sibling checkout already exists, preserve its work and
check the patch with `git apply --check` before applying it. Do not reset an
existing checkout to run these commands. The coordinated server development
workspace prepares this same sibling layout through its setup target.

See [production priorities](../../docs/asset-loop-in-readiness.md) for the
remaining release blockers and validation boundary.
