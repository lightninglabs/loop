# Advanced Custom-Anchor Asset Transactions

## Status

Proposed for cross-team review, with a complete first implementation on the
custom-anchor builder branch. The public API remains experimental until it is
merged and released.

## Summary

Normal Taproot Assets sends should remain address based: the caller identifies
an asset and amount, and tapd owns coin selection, asset transaction creation,
Bitcoin funding, signing, proof delivery, and transfer logging.

Protocol integrations need a separate surface. They may already own a Bitcoin
transaction graph, require BTC-only inputs and outputs beside asset
commitments, choose exact output values and ordering, use external signers, or
keep a fully valid transaction unbroadcast for some time. For those callers,
tap-sdk provides an advanced custom-anchor builder with three explicit
boundaries:

1. an immutable, inspectable plan before any Bitcoin signature;
2. a sealed, persistable package after tapd commits the assets; and
3. a finalization and publish gate that rejects any unsigned-transaction drift.

The SDK owns the public DTOs. Serialized PSBTs, virtual PSBTs, and proofs cross
the public boundary as bytes. Public APIs do not expose taprpc, btcd, or
taproot-assets implementation types.

## Requirements

The first implementation must:

- accept a caller-created anchor PSBT containing additional Bitcoin inputs and
  outputs whose asset inventory is established by the integrating protocol;
- select exact asset inputs from proof files and compare their identity and
  amount with one `AssetRef` field;
- support grouped and ungrouped fungible assets, including multiple concrete
  issuances of the same group;
- assign asset outputs to fixed Bitcoin output indices and exact satoshi
  values;
- support wallet-derived, external, OP_TRUE, and explicit burn asset script
  plans;
- make asset loss and passive-asset handling explicit and fail closed by
  default;
- support caller-funded, tapd-wallet-funded, and external P2A fee-bump anchor
  templates;
- support backend-signed or caller-witnessed virtual inputs;
- expose deterministic key-path, MuSig2, and script-path Bitcoin signing
  requests;
- verify the committed packets and anchor locally before returning data to a
  Bitcoin signer;
- persist every artifact needed to recover after commit or restart;
- support external broadcast through tapd's skip-broadcast log operation; and
- keep gRPC and REST transport behavior equivalent.

It must never silently change an output value, drop asset units, infer a
spending path, ignore a passive asset, or mutate the Bitcoin transaction after
proof suffixes have been created.

## Non-goals

The first implementation does not provide:

- wallet-selected asset inputs or proof locators;
- backend-discovered passive-asset preservation;
- asset-level absolute or relative timelocks;
- a MuSig2 nonce exchange or partial-signature coordinator;
- Bitcoin transaction-graph construction, application policy, or fee
  management;
- an idempotent replacement for tapd's publish-and-log RPC;
- Lightning asset invoices, RFQ, or asset channels; or
- a new embedded asset database or proof server.

## Ownership Boundary

| Concern | tap-sdk | tapd-compatible backend | Integrating protocol |
| --- | --- | --- | --- |
| Public request validation | Owns | No | Supplies policy |
| Proof-file decoding and identity | Local | Confirms chain validity | Supplies proof |
| Asset selection | Exact proof-selected inputs | Future wallet selection | Chooses inputs |
| V1 virtual transaction assembly | Owns | Signs managed inputs | Supplies external witnesses |
| Asset commitments and proof suffixes | Verifies | Creates during commit | Inspects result |
| Bitcoin transaction topology | Preserves and verifies | May add wallet funding and change | Owns inputs, outputs, order, v3/P2A policy |
| Bitcoin signature digests | Computes and verifies | No | Runs signers and MuSig2 sessions |
| Transfer persistence | Defines sealed package | Keeps wallet transfer state | Persists package and state machine |
| Broadcast and transfer log | Calls and validates | Owns RPC behavior | Chooses internal or external broadcast |
| Unconfirmed proof paths | Serializes and verifies asset transitions | Optional compatible verifier | Attests Bitcoin graph and stores paths |
| Deployment | Transport-neutral | Local, remote, or hosted | Chooses trust and availability model |

In particular, tap-sdk does not construct the integrating protocol's
transaction tree. That protocol chooses transaction version, locktime, input
outpoints and sequences, output order, connector outputs, P2A outputs, and
package relationships before asking the asset builder to bind commitments
into that topology.

Every host graph edge that moves an asset must also be a real V1 Taproot Assets
transition consuming the prior asset-bearing outpoint. A Bitcoin-only
replacement or refresh that forfeits an old output while independently funding
a new output cannot carry the asset. When a graph contains intermediate
checkpoints, every asset transition must be built, committed, persisted, and
verified before Bitcoin signatures for that graph are requested.

The MVP still requires a tapd-compatible backend for key derivation, backend
virtual signing when selected, confirmed-proof verification, commit,
publish/log, and wallet funding. That backend can be local, remote, hosted, or
a smaller compatible service. tap-sdk itself is not the complete wallet and
proof runtime.

## Public Model

### Asset identity and selection

Every request uses `AssetRef`. An asset-ID ref identifies one concrete
issuance. A group-key ref identifies a fungible group and may resolve to
several concrete issuances. The plan and package include both the original
`AssetRef` and the resolved issuance ID so persistence and diagnostics are
unambiguous. A concrete asset-ID ref may select one issuance even when that
issuance belongs to a fungible group. Grouped collectibles remain concrete
asset-ID refs; only fungible assets use their group key as the semantic ref.

`CustomAssetInput` selects the complete asset at the tip of exactly one
confirmed proof file or compact proof path. The requested amount must equal
that asset's amount; the builder does not partially select a proof. Confirmed
proof files use backend chain verification. Compact paths use a host-supplied
confirmed-base verifier, host attestation of every unconfirmed Bitcoin
transition, and SDK-local Taproot Assets transition verification. The builder:

1. decodes the proof locally;
2. checks the exact amount and `AssetRef` equivalence locally;
3. rejects asset timelocks;
4. asks the backend to verify the confirmed proof chain; and
5. cross-checks the backend summary with the locally decoded proof.

For compact paths, steps 4 and 5 instead use the explicitly separated host and
local checks described below.

For a confirmed input, verification is not the same as import. Before commit,
the same tapd instance that will eventually publish/log the transfer must
already have the input proof in its proof archive. The current APIs have no
read-only inventory check that can prove this precondition, so the integration
must import selected proofs as a separate, authenticated setup step and retain
the proof source in its recovery state.

For `CustomAssetWitnessBackendSigner`, the builder also queries the local
wallet's complete script-key descriptor and checks that its raw key and
Taproot tweak derive the exact proof-selected output key. It serializes both
the derivation and the tweak into the virtual input before signing. A proof
contains only the output key: recovering a raw key locator alone cannot sign
assets received at unique Pedersen addresses, whose non-empty tweak must be
preserved. Caller-provided witnesses do not require a wallet key lookup.

Local, host-attested, and backend-trusted checks are distinct entries in
`CustomAnchorVerificationResult`.

### Outputs and script plans

Each `CustomAssetOutput` has a stable caller ID, `AssetRef`, amount, fixed
anchor output index, exact anchor value, asset script plan, anchor internal key,
optional tapscript sibling, and proof-delivery metadata.

Supported asset script modes are:

- `CustomAssetScriptWallet`, for a fresh backend wallet key;
- `CustomAssetScriptExternal`, for a complete caller-supplied script key;
- `CustomAssetScriptOPTrue`, for an OP_TRUE asset leaf protected by the
  Bitcoin-level policy; and
- `CustomAssetScriptBurn`, for the protocol burn key derived from each concrete
  virtual packet's input `PrevID`.

When one group-key request consumes multiple issuances, each concrete output
needs a distinct asset commitment key. The builder derives the protocol's
issuance-specific unique script-key tweak from the raw wallet, external, or
OP_TRUE key. Burn allocation temporarily uses the protocol NUMS key, but the
final output uses `asset.DeriveBurnKey` with that packet's concrete input
`PrevID`; this is what makes `Asset.IsBurn` recognize the transition. A
pre-tweaked external key cannot be safely reused for this case and is rejected.

### Passive assets and loss

The zero-value policies are fail-closed:

- `CustomAnchorPassiveReject` requires the active packets to account for every
  asset committed at every spent anchor input.
- `CustomAnchorLossReject` requires exact amount conservation and rejects burn
  outputs.

The current positive passive mode is
`CustomAnchorPassiveCallerReanchor`. The caller supplies every V1 passive
packet, and the SDK validates its identity and amount before including it in
the complete anchor-input commitment check. `CustomAnchorPassivePreserve` is
reserved but returns unsupported because the current tapd commit response
cannot prove that backend discovery was complete before signing.

Irreversible loss requires `CustomAnchorLossBurn`, an explicit confirmation
flag, and a per-`AssetRef` upper bound. Only explicit protocol burn outputs
count against the bound. Input and output amounts must still balance exactly;
implicit or intentionally unallocated deficits are rejected so loss cannot be
hidden outside a verifiable burn transition.

### Funding

`CustomAnchorFundingPlan` is a tagged union:

- `CustomAnchorFundingCallerFundedExact` prevents tapd from changing the
  number of inputs or outputs;
- `CustomAnchorFundingExternalP2AFeeBump` has the same exact-parent behavior
  and additionally requires a version-3 transaction with the canonical
  zero-value P2A output at the declared index, an exact zero parent fee, and no
  other dust output; and
- `CustomAnchorFundingWalletFunded` lets tapd append wallet inputs and add or
  update one declared change output using an explicit fee and lock policy.

For wallet funding, the SDK accepts only a suffix of newly added inputs and at
most one returned change output. Existing input outpoints and sequences,
non-change BTC outputs, all output ordering, and every asset output value must
remain unchanged. The caller must also provide a non-zero `MaxFeeSat`. The SDK
computes the exact committed fee from all PSBT prevouts and rejects a backend
result above that hard ceiling before any Bitcoin signer sees it. The actual
fee and ceiling are sealed into the durable package and independently
recomputed during package validation.

The high-level no-new-change mode is capability gated and is not enabled in
the pinned tapd profile. Although tapd's protobuf currently represents this as
an `add=false` variant, the downstream lnd funding request does not preserve a
distinct no-change instruction. Treating that shape as supported would let a
backend add a change output after the caller had selected an exact topology.

P2A is a caller-side Bitcoin policy. The SDK validates the parent shape,
including the exact zero parent fee and absence of any second dust output; it
does not claim that tapd performs package relay or fee-bump construction. A
P2A funding plan always seals skip-broadcast and external-broadcast metadata,
and commit preflights tapd's skip-broadcast capability before any signer sees
the parent. The host constructs, signs, and submits the fee-paying child as a
package through Bitcoin Core or an equivalent package-relay surface.

## Lifecycle and Frozen Transaction Invariant

There are three different PSBT states and they must not be conflated:

1. **Caller template.** The protocol supplies complete transaction topology.
   Asset-bearing output scripts may still be placeholders. In wallet-funded
   mode, tapd may later append inputs and one change output.
2. **Committed anchor.** tapd funds when requested, inserts the asset
   commitment scripts, and creates proof suffixes in one commit operation.
   The SDK locally checks the returned virtual packets against the complete
   returned anchor and seals it into a transfer package.
3. **Final anchor.** Signers may add PSBT signature and final-witness fields.
   The unsigned transaction must be byte-for-byte equivalent to the committed
   anchor.

The committed unsigned transaction is frozen. The following ordered fields
are all part of the invariant:

- version and locktime;
- every input outpoint and sequence;
- every output value and script; and
- input and output ordering.

Changing any of those fields changes the transaction ID or the commitment
context and invalidates the returned proof suffixes. In particular, v3 and P2A
must be present in the caller template, and no code may normalize all input
sequences after commit.

The concrete lifecycle is:

1. The caller builds a `CustomAnchorRequest` and the full anchor template.
2. `Build` validates the request, resolves and verifies proof-selected inputs,
   allocates grouped issuances, prepares V1 virtual packets, validates
   caller-provided virtual witnesses, merges required PSBT metadata, and
   returns an immutable `CustomAnchorPlan`.
3. The caller inspects `plan.Verification()`, `plan.AnchorPSBT()`, and the
   prepared virtual packets. No Bitcoin signature has been requested.
4. The caller can use `PreviewOutputCommitments` to derive the exact Taproot
   Asset and BIP341 output roots without signing or committing. This lets a
   host compose and canonically order final Bitcoin output scripts before it
   freezes the transaction.
5. `Commit` checks required backend capabilities. It asks the backend to sign
   virtual packets whose inputs use the backend-signer mode, then calls
   `CommitVirtualPsbts`.
6. The SDK validates the backend's funding shape and runs
   `ValidateAnchorInputs` and `ValidateAnchorOutputs` over the complete active
   and passive packet set. It returns a sealed
   `CustomAnchorTransferPackage`.
7. The caller persists the package before external signing, broadcasting, or
   handing it to another service.
8. The caller derives typed Bitcoin signing requests from the package, applies
   external signatures or script witnesses on returned package clones, and
   invokes an `AnchorSigner` for wallet-managed Bitcoin inputs when needed.
9. `VerifyFinalAnchorPSBT` compares the complete unsigned transaction and all
   non-signature PSBT metadata with the frozen committed transaction.
10. `Wallet.PublishCustomAnchorTransfer` additionally requires a fully
   finalized PSBT, revalidates all asset commitments, checks skip-broadcast
   capability, and calls tapd's publish-and-log operation.

Caller-provided virtual asset witnesses necessarily exist before commit;
Bitcoin anchor signatures necessarily come after commit. Moving either to the
wrong side of that boundary creates invalid commitments or asks a Bitcoin
signer to authorize an unverified transaction.

The first caller-witness API is intentionally limited to a complete witness
that is already valid for the packet produced by `Build`, such as a static
OP_TRUE path controlled by the integrating protocol. Interactive asset signing
after packet assembly requires a separate immutable asset-signing request/apply
phase and is follow-up work.

Taproot Asset V1 output commitment leaves use witness-stripped asset encoding,
so `PreviewOutputCommitments` does not need backend virtual-input signatures.
It operates on decoded copies, so deriving the deterministic spent-output
alternate leaves cannot mutate the immutable plan. For split transitions the
commitment root depends on the virtual output's anchor index. A host that uses
the preview to sort equal-value Bitcoin outputs must therefore rebuild and
preview after reindexing, then require the final permutation to be stable
before commit.

## Durable Transfer Package

`CustomAnchorTransferPackage` is the recovery boundary. It stores:

- a schema version and immutable plan ID;
- the original committed anchor PSBT and the current signed/finalized clone;
- committed active and passive virtual PSBTs;
- change index and the locked outpoints actually returned by tapd;
- the selected funding mode, P2A index when applicable, exact committed fee,
  and wallet-funding fee ceiling;
- the caller's custom lock ID and requested expiration, when wallet funding
  was used;
- logical-to-concrete input and output mappings;
- each output's Taproot Asset commitment root and final BIP341 merkle root,
  derived from the complete committed active/passive packet set rather than
  trusted proprietary PSBT fields;
- the self-contained confirmed proof file or compact proof path for every
  selected input, bound by a domain-separated content ID;
- every output proof suffix and complete proof-delivery metadata;
- explicit external signing plans and backend-managed input indices; and
- publish label, skip-broadcast choice, and external-broadcast intent.

It uses three independent digests:

- `UnsignedTxDigest` binds the complete ordered unsigned transaction;
- `CommittedPackageDigest` binds the immutable commit result while excluding
  mutable PSBT signature/finalization fields; and
- `PackageDigest` detects any mutation of the current persisted snapshot.

`Seal`, JSON encoding, and binary encoding are strict and versioned. Unknown
versions, trailing bytes, inconsistent signing classifications, duplicate
mappings, digest changes, and tampered PSBTs fail validation. All accessors and
signature application methods return deep copies so a previously persisted
snapshot cannot change through caller aliasing.

Public JSON and binary decoding is atomic: bytes are decoded and validated in
a temporary value, and a failure leaves the receiver unchanged.

Funding metadata is mode-strict. Caller-funded exact and external P2A packages
cannot carry backend change, lock, locked-UTXO, or backend-managed-input
metadata; wallet-funded packages require their deterministic custom lock ID.

For a host-owned policy tree, such as an Ark VTXO, the host combines its known
policy root with `CustomAnchorAssetOutputSummary.TaprootAssetRoot`. The result
must equal the summary's `TaprootMerkleRoot`; that final root is the tweak
committed by the anchor output key. The SDK exposes both values so the host can
build and persist its control blocks without parsing tapd's proprietary PSBT
output fields. The host remains responsible for persisting its complete policy
descriptor, leaves, signers, and internal key. The SDK also requires tapd's
PSBT root hints to be present and equal to these independently reconstructed
values because the downstream transfer logger persists those hints.

The committed PSBT metadata is also sealed. Before finalization, the current
PSBT may differ only in signature and finalization fields; changes to prevout,
sighash, Taproot key/leaf, or other signing metadata are rejected. Standard
PSBT serialization removes non-final input metadata after finalization, so the
SDK reconstructs the expected finalized packet from the committed metadata and
the final scripts, compares its canonical encoding, and restores the committed
metadata in the persisted package.

These unkeyed digests detect accidental corruption, stale snapshots, and
inconsistent state. They are not an authenticity mechanism against a party
that can rewrite a package and recompute its digests. A hostile-storage threat
model requires the host to bind the expected plan/committed digest in an
authenticated database record, MAC, or signature outside the package.

The tapd commit RPC currently returns only locked outpoints. The SDK preserves
the caller-supplied lock ID and requested expiration and verifies that returned
locks exactly cover backend-added inputs, but it does not invent per-output
lease IDs, values, or actual expirations that tapd did not return.

## Bitcoin Signing

Every caller-supplied anchor input must select exactly one trusted spend path:

- single-signer Taproot key path;
- aggregate MuSig2 key path; or
- one exact Taproot script leaf.

Inputs appended by wallet funding are classified separately as
backend-managed. The two sets are an exact, disjoint cover of all committed
anchor inputs.

Caller-supplied inputs are P2TR-only in the first implementation because every
such input selects a typed key-path, MuSig2, or script-path plan. Inputs
appended and signed by the backend in wallet-funded mode may use native
P2WPKH, P2WSH, or P2TR. All inputs must have an empty scriptSig before commit.
Legacy and wrapped-SegWit inputs can acquire a scriptSig during finalization and
therefore change the txid after proof suffixes are created; the builder rejects
them.

`SigningRequests` derives BIP341 sighashes from the committed transaction and
all PSBT prevouts. Requests include a digest-bound ID, input index, prevout,
prevout value and script, sighash type and digest, internal key, merkle root,
and mode-specific material. Script-path requests additionally include the leaf
hash, version, script, control block, and declared signers.

The SDK verifies that each explicit plan matches the committed PSBT. Signature
application is immutable and cryptographic:

- key-path application accepts only a valid Schnorr signature for the declared
  signer or aggregate MuSig2 key and exact sighash;
- script-path application checks the leaf and control block and executes the
  complete final witness under the standard Bitcoin script flags;
- finalized external inputs must use the exact sealed key path or selected
  leaf, the exact sealed sighash, and every 64/65-byte script witness signature
  must match a declared required signer; and
- request IDs cease to match if any committed field changes.

The first script-path surface rejects annexes and selected leaves containing
an actual `OP_CODESEPARATOR`, because either feature changes sighash semantics
that the typed request does not model. Opcode parsing distinguishes a real
separator from the same byte inside pushed data. Backend-managed inputs are
still fully consensus-validated but are not constrained to an external signing
plan.

The SDK intentionally does not own MuSig2 nonce generation, nonce exchange,
partial-signature aggregation, or session storage. The integrating protocol
owns those rounds and applies only the final aggregate Schnorr signature.

## Verification and Trust

| Check | First implementation |
| --- | --- |
| Request structure, tagged unions, limits | Local SDK |
| Amount conservation and bounded loss | Local SDK |
| Proof decoding, identity, amount, asset type | Local SDK |
| Confirmed proof-file chain validity | Backend trusted and marked as such |
| Compact-path confirmed base and inventory | Configured host verifier |
| Unconfirmed Bitcoin graph authorization and policy | Integrating host |
| Unconfirmed asset VM and commitment transitions | Local SDK |
| Complete active/passive anchor-input inventory | Local SDK from supplied packets |
| V1 virtual transaction and caller witness VM rules | Local SDK |
| Anchor output commitment, index, script, value | Local SDK after commit |
| Backend funding shape and change constraints | Local SDK |
| Signing-plan and PSBT compatibility | Local SDK |
| BIP341 sighash and applied signatures/witnesses | Local SDK |
| Final transaction and immutable PSBT metadata equality | Local SDK |
| Transfer logging and wallet state mutation | Backend |

The backend verification marker is not a claim of local trustlessness. A
remote or hosted backend can verify a confirmed proof for a lightweight client,
but the caller must decide whether that backend is trusted for chain state.

## Compact Unconfirmed Proof Paths

A normal proof file repeats its entire history. Repeating the full file at
every node of a deep off-chain transaction graph grows storage quadratically
and still does not make unconfirmed anchors chain-valid. The SDK therefore
defines `AssetProofPath` for the restricted unconfirmed case.

A path contains one confirmed base proof file and an ordered list of serialized
V1 transition proofs. The container has a deterministic checksummed binary
encoding, depth and byte limits, deep-copy semantics, and domain-separated
content IDs for the full path and each step. A store can keep steps once by
content ID and let VTXO records refer to a parent path plus one new step.

A step that merges several prior states carries the evidence for its extra
inputs alongside the spine as complete recursive co-input paths. Each co-input
path has its own confirmed base and unconfirmed steps. A per-step path count,
a nesting-depth limit checked before nested content is parsed, and one shared
byte budget bound the complete structure.

Verification proceeds as follows:

1. A `ConfirmedProofVerifier` fully verifies the base proof against the chain
   and attests that the base anchor inventory is complete and contains no
   passive assets. Every co-input path is resolved recursively with the same
   engine, so its confirmed base receives the identical treatment.
2. The SDK decodes the base tip and every transition.
3. For every unconfirmed step, the verifier must also implement
   `UnconfirmedAnchorVerifier`. The host validates the complete serialized
   anchor transaction against its authoritative Ark/swap graph, including the
   expected previous and new outpoints — a merging step reports the complete
   input set in `PreviousAnchorOutpoints` — all Bitcoin prevouts and
   signatures, standardness, and transaction/package policy.
4. For each step the SDK checks exact parent asset identity, outpoint, amount, and
   script-key continuity, and checks every co-input tip against the same
   immutable identity.
5. It binds the step's effective witness set (the root asset's witnesses for
   split transitions) to exactly the spine tip plus the verified co-input
   tips, and requires the anchor transaction to spend each of those outpoints
   exactly once — the native verifier only proves the spine prevout is
   consumed.
6. It runs the native Taproot Assets proof verifier with chain-inclusion
   verification skipped only for the unconfirmed transition, splicing each
   co-input lineage in as a synthetic in-memory proof file built from the
   co-path's own declared proofs. Asset VM, commitment, witness, inflation,
   and state-transition checks still run.
7. It reconstructs the selected output commitment, including disclosed
   alternate leaves, and compares the exact anchor output script. This rejects
   hidden co-anchored active or passive assets.

The host attestation is a mandatory trust boundary for non-empty paths, not an
optional safety enhancement. Native Taproot Assets proof verification does not
validate the complete Bitcoin input graph, Bitcoin witnesses, standardness, or
Ark package policy. The SDK therefore fails closed if the confirmed verifier
does not provide the unconfirmed-anchor extension or rejects any step. The
inspectable result records host-attested chain/graph checks separately from
SDK-local asset-transition checks.

An edge accepts one active asset predecessor per declared input: the spine
plus one per co-input path. A Bitcoin transaction may contain additional
opaque Bitcoin or connector inputs. An empty Taproot Assets
`AdditionalInputs` field on a step's transition proof proves only that those
inputs do not contribute to the tracked transition; the compact path verifier
cannot attest their full asset inventory. The host's
`UnconfirmedAnchorVerifier` must establish that inventory before treating
them as BTC-only connectors. Any step may merge several inputs into one
output and may split its inputs into several asset outputs — including the
Ark merge-and-split shape — as long as every input is evidenced by the
confirmed base or a declared co-input path; the selected split child must
carry its split-root proof and the root asset must carry a complete V1
witness per input. Transition proofs arriving with embedded input files,
asset timelocks, ownership challenges, non-V1 assets, and passive assets are
rejected.

These paths are an application-level unconfirmed representation. They must not
be imported into tapd or a universe as confirmed proofs. Once the relevant
branch confirms, the integration obtains or materializes ordinary
chain-anchored proofs, verifies them normally, imports/registers them, and may
replace the compact path with a new confirmed base. A public SDK helper for
materializing every transition from block headers and merkle proofs is future
work; the current upstream `proof.UpdateTransitionProof` primitive uses
implementation types and is intentionally not leaked through this API.

An unconfirmed path package cannot be passed to
`PublishCustomAnchorTransfer`. Today tapd's chain porter verifies every input
proof with full header and merkle inclusion before broadcast and then fetches
the proof prefix from its local archive. There is no skip-chain or proof-path
request field. The SDK therefore rejects this call locally. Publishing such a
branch needs either a path-aware tapd RPC or a post-confirmation materialize,
import, rebuild/rebind, recommit, and reseal flow.

## Capabilities and Transport

The capability model is an optional extension to `WalletKitClient`, not a new
mandatory method on every SDK client. Each capability is supported,
unsupported, or unknown; both unsupported and unknown fail closed when the
operation requires the feature.

Current tapd has no live capability RPC. The gRPC and REST clients therefore
return an SDK-version-pinned compatibility profile. This is an explicit
assumption about the tapd API version, not runtime discovery. Custom backends
should implement `CustomAnchorCapabilityProvider` with their real profile.

The legacy `WalletKitClient` commit and publish method signatures remain
source-compatible. Advanced request DTOs are exposed through the optional
`CustomAnchorWalletKitClient` extension implemented by the SDK gRPC and REST
clients. Custom-anchor lifecycle calls fail closed with an explicit unsupported
capability error when a third-party client lacks that extension.

Both transports support equivalent SDK DTOs for:

- caller-supplied anchor PSBTs;
- active and passive virtual packets;
- skip funding, change selection, fee targets/rates, and lock parameters;
- returned change index and locked outpoints; and
- finalized publish/log with skip broadcast and label.

TLS, macaroon, timeout, and deployment configuration remain transport
concerns. The root package never marshals taprpc types.

## Failure and Recovery

The host application should persist state at least at these points:

1. plan accepted;
2. committed package persisted;
3. signed/finalized PSBT persisted;
4. external broadcast submitted or result unknown;
5. publish/log completed or result unknown; and
6. proof delivered/imported.

`CommitVirtualPsbts` can lock wallet UTXOs. A verified response records the
returned outpoints, so abandoned packages require explicit lock cleanup using
those outpoints and the backend's lease API.

A transport timeout is harder: tapd can successfully commit and retain lnd
leases before its response reaches the caller. In that case no committed
package or returned outpoint inventory exists even though locks may remain.
Wallet-funded custom-anchor requests therefore require a deterministic 32-byte
custom lock ID. The integration must persist it before the RPC and treat every
unknown commit result as a reconciliation state rather than a safe retry. Today
that reconciliation must inspect lnd leases out of band. A robust backend
contract needs an idempotent commit request ID and status lookup, or at minimum
list and release operations keyed by the custom lock ID.

The SDK returns `CustomAnchorCommitAttemptError` for a failed call or unusable
response and sets `OutcomeUnknown` whenever the backend may have committed.
Callers must branch on that field: an unknown outcome enters reconciliation,
while a known pre-execution rejection may be safely corrected before a new
attempt.

Two existing rollback paths also require upstream fixes. lnd's multi-output
funding rollback releases earlier leases with its internal lock ID even when
the caller supplied a custom ID. tapd's post-funding rollback reuses the
original request context, which can already be canceled when cleanup runs.
Either failure can leave leases until expiry and must not be described as
recoverable by the SDK alone.

tapd's `PublishAndLogTransfer` has no request ID or documented idempotency
contract. A timeout after submission is therefore an ambiguous result. The
integration must not blindly retry. It should first inspect the Bitcoin
mempool/chain and tapd transfer history, reconcile by transaction ID, and only
then decide whether to resubmit or continue proof recovery. Adding a
duplicate-safe request ID and status lookup is recommended upstream work.

Publish/log failures use `CustomAnchorPublishAttemptError`. A nil response, a
response without an anchor transaction, a response whose anchor or virtual
packets differ from the finalized sealed package, a transport error that may
have reached the backend, and `AlreadyExists` all set `OutcomeUnknown` because
none proves that a prior operation used identical content. These cases require
the same transaction-ID reconciliation before retry.

## API Examples

Normal users stay on the address-based wallet surface:

```go
transfer, err := wallet.Send(ctx, address, tapsdk.WithAmount(100))
if err != nil {
    return err
}
```

Only integrations that own the Bitcoin topology use the advanced lifecycle.

The exact transaction topology is intentionally created outside the builder:

```go
builder := wallet.NewCustomAnchorTxBuilder()

plan, err := builder.Build(ctx, &tapsdk.CustomAnchorRequest{
    Inputs: []tapsdk.CustomAssetInput{input},
    Outputs: []tapsdk.CustomAssetOutput{output},
    AnchorPSBT: anchorTemplate,
    Funding: tapsdk.CustomAnchorFundingPlan{
        Mode: tapsdk.CustomAnchorFundingExternalP2AFeeBump,
        ExternalP2AFeeBump: &tapsdk.CustomAnchorExternalP2AFeeBump{
            P2AOutputIndex: p2aIndex,
        },
    },
    PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
        Policy: tapsdk.CustomAnchorPassiveReject,
    },
    LossPolicy: tapsdk.CustomAnchorLossPolicy{
        Mode: tapsdk.CustomAnchorLossReject,
    },
    SigningPlans: []tapsdk.CustomAnchorInputSigningPlan{signingPlan},
})
if err != nil || !plan.Verification().Valid() {
    return err
}

committed, err := plan.Commit(ctx, tapsdk.CustomAnchorCommitOptions{
    Publish: tapsdk.CustomAnchorPublishMetadata{
        SkipAnchorTxBroadcast: true,
        ExternalBroadcast: true,
    },
})
if err != nil {
    return err
}
if err := persist(committed); err != nil {
    return err
}

requests, err := committed.SigningRequests()
if err != nil {
    return err
}

// This example has one external key-path input. Repeat the corresponding
// Apply call for every key-path, MuSig2, and script-path request, always using
// the package clone returned by the previous call.
signature, err := keyPathSigner(ctx, requests.KeyPath[0])
if err != nil {
    return err
}
signed, err := committed.ApplyKeyPathSignature(
    requests.KeyPath[0].ID, signature,
)
if err != nil {
    return err
}
if err := persist(signed); err != nil {
    return err
}

finalPSBT, err := anchorSigner(ctx, signed.AnchorPsbt)
if err != nil {
    return err
}
finalized, err := signed.WithFinalAnchorPSBT(finalPSBT)
if err != nil {
    return err
}
if err := persist(finalized); err != nil {
    return err
}

packet, err := wallet.PublishCustomAnchorTransfer(
    ctx, finalized, finalized.AnchorPsbt,
)
```

## Test Strategy

The implementation requires four layers of tests:

- DTO validation, cloning, strict serialization, overflow, passive/loss, and
  capability unit tests;
- cryptographic tests for V1 state transitions, grouped multi-issuance splits,
  output commitment reconstruction, BIP341 digests, signature/witness
  application, and tamper rejection;
- gRPC and REST mapping tests over identical low-level request/response DTOs;
  and
- real tapd/lnd regtests for an exact caller-funded v3 anchor and an external
  zero-fee P2A parent through both gRPC and REST. The exact flow preserves a
  non-default sequence. The P2A flow externally funds and signs the child,
  submits parent and child as a package, confirms them, exports/imports the
  receiver proof, and verifies the receiver balance.

Negative tests must mutate every frozen transaction field, omit passive
packets, exceed loss bounds, provide mismatched proofs or script keys, mix
virtual witness modes, use unknown capabilities, tamper with package digests,
and corrupt compact proof paths.

## Upstream and Follow-up Work

The current MVP can support confirmed, proof-selected protocol transactions
without a tapd protocol change when every selected input proof is already in
the publishing tapd archive. It can also build, verify, and persist compact
unconfirmed paths, but current tapd cannot publish/log those packages. The
following improvements should be tracked separately:

1. **Multi-packet wallet funding.** `FundVirtualPsbt` rejects a response with
   more than one packet, while grouped multi-issuance selection can naturally
   require one packet per concrete issuance. This blocks wallet-selected group
   inputs, not the proof-selected MVP.
2. **Idempotent publish/log.** Add a request ID and a transaction-status lookup
   or documented duplicate-safe semantics to `PublishAndLogTransfer`.
3. **Commit lease recovery.** Fix lnd's partial funding rollback to release
   each output with its actual custom lock ID, and make tapd cleanup use an
   independent bounded context. Then make `CommitVirtualPsbts` idempotent and
   expose status by request or lock ID. Also expose enough lease metadata to
   list and release every retained lnd lock after a lost response. Until then,
   a commit timeout is an externally reconciled, outcome-unknown state.
4. **Explicit no-change funding.** Preserve a real no-new-change policy through
   tapd into lnd instead of encoding it as an `add=false` value whose semantic
   distinction is lost. The pinned capability profile intentionally reports
   this feature as unknown.
5. **Live capabilities.** Expose supported custom-anchor features and API
   version through tapd `GetInfo`; remove the static compatibility profile when
   available.
6. **Supported V1 witness rebinding.** Expose a stable helper for rebinding a
   complete V1 witness to a new virtual transaction instead of requiring
   integrations to manipulate protocol structs.
7. **Path-aware proof APIs.** If tapd is to own off-chain paths, add explicit
   verify/store/finalize operations that distinguish unconfirmed transitions
   from confirmed proof import.
8. **Granular macaroons.** Split advanced commit, log-only publish, proof
   verification, and proof import permissions for hosted deployments.
9. **Proof materialization helper.** Add an SDK-owned confirmation DTO and path
   finalizer after the required chain-data source and retry model are agreed.
10. **Path-aware publish or rebinding.** Accept verified compact proof sources
   in chain porter, or define a post-confirmation API that rebinds materialized
   input proofs and reseals the package without leaking implementation types.
11. **VerifyProof predecessor metadata.** tapd currently marshals
   `VerifyProofResponse.DecodedProof` without predecessor IDs, while
   `DecodeProof` can expose them. The builder locally decodes proof files and
   does not depend on this response field, but integrations must not infer
   predecessor inventory from the `VerifyProof` summary. Upstream should expose
   the metadata or explicitly document its omission.

The deployment follow-up must add a tapd-compatible service to the integrating
product's Kubernetes chart with pinned versions, network/chain agreement, TLS
and macaroon secret distribution, persistent proof and wallet state, backup,
network policy, health checks, and monitoring. That belongs in deployment
infrastructure, not in tap-sdk.

## Rejected Approaches

- Mutating a serialized anchor after commit, including appending P2A/change,
  changing v3, or normalizing sequences. This invalidates transaction IDs and
  proof suffixes.
- Overwriting an asset-bearing output value with the total asset input amount.
  Asset units and satoshis are independent dimensions.
- Hard-coding one asset ID per transaction. Group-key sends can span multiple
  concrete issuances.
- Inferring the safe Taproot spend path from PSBT fields. The caller must select
  the exact trusted path.
- Treating one selected inclusion proof as evidence that a spent anchor had no
  other assets. Complete packet inventory or an explicit inventory attestation
  is required.
- Copying the full proof prefix into every off-chain node. Content-addressed
  path steps preserve verification while avoiding quadratic storage.
- Making capability discovery a mandatory method on `WalletKitClient`. Existing
  clients must remain source compatible and unknown support must fail closed.
