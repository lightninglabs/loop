# Loop asset transaction operations

Bitcoin deposit and HTLC contracts live in `github.com/lightninglabs/loop/swapkit`.
This package connects those contracts to the Taproot Assets SDK and the existing
LND signer, chain services and tapd wallet. It has no separate contract types,
transaction format, persistent signer or swap lifecycle.

## Contracts and SDK packages

The asset leaf is `OP_TRUE`. Bitcoin enforces the deposit's cooperative MuSig2
path and client CSV refund, or the HTLC's preimage claim and timeout refund.
The normal static Loop In uses the V3 CLTV refund policy. Contract scripts,
control blocks and witness assembly are tested in `swapkit`.

Use the base contract constructors directly. `AnchorPlan`, `OPTrueOutput`,
`CooperativeSigningPlan` and `ScriptSigningPlan` only convert those contracts
into SDK request fields. The SDK builder validates allocations, selects assets,
constructs commitments and returns digest-bound signing requests. It creates
unique asset witnesses for each concrete output; never reuse a witness across
issuances in a fungible group.

Unsigned and finalized transfers use `CustomAnchorTransferPackage` directly.
Persist its `MarshalBinary` bytes and restore them with `UnmarshalBinary`.
`WithFinalAnchorPSBT` incorporates the final witness into the same package and
verifies consensus execution. Accepting a peer's finalized transfer also requires
matching the approved `CommittedPackageDigest`; a valid self-sealed package or
matching transaction ID alone does not establish authorization. There is no
separate transaction or recovery envelope.

A public operation validates the negotiated deposit contract and proof once,
then passes that result to its internal helpers. The checks include asset/group,
amount, outpoint and Bitcoin policy. Current-chain checks bracket external proof
verification, so a chain change during that call fails the operation. A later
operation or restart validates again. SDK package validation and final witness
verification remain separate requirements.

## Signing, publication and proofs

`Driver` holds existing node-service dependencies. LND owns keys and ephemeral
MuSig2 sessions. An interrupted signing round starts with fresh nonces for the
same unsigned package; secret nonces are never persisted. The signing bridge
translates SDK signing requests to LND calls and applies the returned signatures
through the SDK.

`BuildDeposit` and `BuildDepositRefund` return complete signed packages without
publishing them. The ordinary deposit FSM saves each package on its record
before publication. The ordinary Loop In record similarly holds its recovery
package and completed fallback. No extra journal or recovery worker is needed.

Before publishing, import selected confirmed input proofs into the publishing
tapd's archive. External OP_TRUE keys must first be declared with their exact
script tree and tweak. This lets tapd reconstruct successor proofs; it does not
give tapd either party's Bitcoin key. Export proofs by exact issuance, script
key and outpoint, and verify their policy, current chain and spendability.

For an offline peer's already-mined spend, reconstruct the known package, verify
the exact transaction and witness, and log it with broadcasting disabled. Tapd
then reconstructs the confirmed successor proof. LND restores spend notices
from the deposit height hint. A Bitcoin RPC callback supplies the mempool-aware
UTXO check for anchors outside LND's wallet inventory.

## Execution and restart recovery

The server owns the preimage and execution decision. It prepares the same-group
asset payout to the client's HOLD invoice. The client independently verifies the
accepted native HTLC and saves the complete fallback before sending claim
authority. The server saves its decision before revealing the preimage. These
ordering rules belong to the existing FSM actions, not the SDK.

An OP_TRUE ownership proof cannot establish exclusive ownership. Signing requires
complete verified transfer history and authenticated assignment of the deposit.
The client retains unilateral recovery if the server does not execute.

The experimental database and package encoding are not released formats. This
revision replaces earlier draft JSON envelopes; recreate databases containing
those old packages. Restart tests cover packages saved in the current format.

## Development and validation

Prepare the compatible SDK and run tests from the normal Loop module:

```sh
scripts/prepare-tap-sdk.sh
go test ./swapkit/... ./staticaddr/... ./assets/assetln ./loopd
```

The temporary SDK base is `932b4aa5429b66df2df13fcfac03fc6c6bdf974e`, before the
split btcd v2 imports. The preparation script applies the generic metadata fixes,
compressed MuSig2 participant support and current proof rules against Taproot
Assets v0.8.6. SDK source is not copied into this package. Replace development
patches with reviewed compatible releases before production.

Adapter tests cover SDK commitment conversion, finalized-package binding,
chain freshness and signing. `itest/TestSwapKitDepositLifecycle` exercises the
SDK lifecycle against tapd. The server's production process tests cover the
normal client/server flow, controlled execution, restarts and unilateral refunds.
See [production priorities](../../docs/asset-loop-in-readiness.md) for remaining
release blockers.
