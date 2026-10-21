# Asset transaction policy helpers

The ordinary deposit and static Loop In FSMs own asset transactions. They call
`github.com/lightninglabs/tap-sdk` directly to build and commit transfers, use
their existing LND services to sign, and save completed packages before exposing
signatures or publishing. This package has no driver, client, manager, signer
service, journal or lifecycle.

The remaining helpers bind SDK inputs and outputs to Loop's Bitcoin contracts,
check the current chain, and reconstruct an offline peer's asset proof from its
observed Bitcoin spend. `DepositContext` and `SwapContext` contain only the data
validated for the current action. They do not retain services or validation
across actions or restarts.

## SDK and contract responsibilities

The asset leaf is `OP_TRUE`. Bitcoin enforces the deposit's cooperative MuSig2
path and client CSV refund, or the V3 HTLC's preimage claim and CLTV refund.
Contract scripts, control blocks and witnesses live in the base `swapkit`.
Small conversion functions populate native SDK request fields.

The SDK owns generic proof verification, allocation and commitment construction,
signing requests, transfer encoding and final Bitcoin witness verification.
Unsigned and completed transfers use `CustomAnchorTransferPackage.MarshalBinary`
and `UnmarshalBinary`. Accepting a peer's completed transfer requires the saved
`CommittedPackageDigest` and `WithFinalAnchorPSBT`; a matching transaction ID or
self-consistent package alone does not establish authorization.

For a building action, inspect Loop policy and the current chain, prepare the
input proof archive, call the native SDK builder, then call `CheckInputsCurrent`
before committing. SDK Build verifies each input proof once. A validation-only
action uses `VerifyDeposit` instead. Current-chain checks bracket verification,
so a reorganization or mempool spend during the SDK call cannot authorize a
stale input. Never add a cross-operation verification cache.

Loop also checks the exact asset group, quantity, issuance, script key, Bitcoin
contract and absence of hidden co-anchored assets. The pinned SDK's passive-asset
reject policy does not establish that last property for ordinary proof-file
inputs; `isolatedRoots` reconstructs the complete disclosed asset commitment.

## Signing and recovery

The existing static-address MuSig2 helper accepts the complete Taproot root,
including the asset commitment. FSM actions use normal LND sessions directly.
An interrupted round starts with fresh nonces for the same unsigned SDK package;
secret nonces are never persisted. `FinalizeScriptSpend` only translates an SDK
script signing request into the existing LND signer call and verifies the
returned witness through the SDK.

Tapd owns publication and successor-proof reconstruction. Input OP_TRUE keys
are declared with their exact script tree, and selected input proofs are
imported when missing. Output registration preserves the native packet's key
parity and locator so later wallet spends derive the correct key. Proof
exports use exact issuance, script key and
outpoint. For an offline peer's already-mined spend, reconstruct the known
package, verify the observed Bitcoin transaction and witness, and log it with
broadcasting disabled. Existing LND spend notifications and Bitcoin backend
UTXO checks provide current-chain evidence.

The server owns the preimage and execution decision. The client verifies the
same-group native HTLC and saves the completed fallback before returning claim
authority. The server saves its decision before revealing the preimage. These
ordering and restart rules belong to the ordinary FSM actions.

## Validation and development dependency

Run `scripts/prepare-tap-sdk.sh`, then test the normal modules:

```sh
go test ./swapkit/... ./staticaddr/... ./assets ./loopd
```

The temporary compatible SDK base is
`932b4aa5429b66df2df13fcfac03fc6c6bdf974e`, before split btcd v2 imports. The
preparation script applies metadata fixes, compressed MuSig2 participant support
and proof rules for Taproot Assets v0.8.6. SDK source is not copied here. Replace
development patches with reviewed compatible releases before production.

Earlier PR drafts used transaction/payment wrappers and duplicate fallback
fields. Recreate databases containing those formats; restart tests cover the
current native SDK package, payment request and normal SQL records. See
[production priorities](../../docs/asset-loop-in-readiness.md).
