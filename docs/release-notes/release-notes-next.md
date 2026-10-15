# Loop Client Release Notes

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

* Shared asset HTLCs now expose the direction-neutral `AssetHTLCV0` policy for
  both Asset Loop In and Asset Loop Out. `LegacyDepositV0` remains an alias
  with the same policy value and on-chain contract terms.

#### Breaking Changes

* Shared deposit kits now require trusted asset genesis metadata, and the group
  key for grouped assets, when creating or validating addresses. Proof-only
  operations continue to accept kits constructed with just the asset ID.

* Shared asset HTLC and deposit kit constructors now require explicit sender
  and receiver internal and script keys. Cooperative spends aggregate only
  internal keys; claims and refunds use script keys. Reusing each party's key
  for both roles preserves existing contracts.

* Shared asset witness builders now require complete, caller-approved virtual
  transfers. Deposit refunds also require the CSV sequence to be set before
  signing any inputs. Construct transfers with `sweep.NewTransfer` to snapshot
  the approved Bitcoin output values, scripts and order before signing.

* Remove the ambiguous deposit `Kit.VerifyProof` method. Use the amount-bound
  `AnchorRootFromProofCommitment` for previously verified proofs, or
  `VerifyProofFile` to verify a complete history and bind the deposit contract.

* `SwapKit.CreateHtlcVpkt` now returns a non-interactive funding template that
  matches V1 HTLC addresses for exact-value and change sends. Integrations
  using the original interactive template must explicitly call
  `CreateInteractiveHtlcVpkt` and validate the actual anchor proof.

* Shared asset HTLC witness builders now require complete, caller-approved
  virtual transfers. Construct transfers with `sweep.NewTransfer` to snapshot
  the approved Bitcoin output values, scripts and order before signing.

#### Bug Fixes

* Shared Taproot Asset sweep packets now preserve the destination address
  version and use the required non-interactive split-root layout. Every input's
  group key must match the destination address, so the recipient's Bitcoin
  script matches the address.

* Shared asset sweeps validate asset conservation and Bitcoin output commitments,
  including change, passive assets, and the STXO and spender leaves required by
  Taproot Assets v0.8.5, before signing. Fresh inclusion and exclusion proofs
  check the complete Bitcoin transaction, rejecting extra outputs that would
  invalidate asset proofs. New deposit addresses use
  explicit asset V1 and address V1 versions, refund signing preserves existing
  signatures, and returned control blocks no longer expose mutable kit keys.

* Deposit proof verification now requires genesis-rooted histories and rejects
  ownership-only proofs, including those hidden in additional inputs. It also
  enforces the Taproot Assets v0.8.5 proof activation rules on every proof and
  bounds the nesting depth and decoded size of additional inputs, which
  previously let a small proof file force gigabytes of decoding work.

* Shared asset HTLC and deposit proof acceptance now requires the proof to
  describe the complete anchor commitment. A funder could otherwise hide an
  extra leaf in the anchor output and block the receiver's claim after the
  receiver paid the invoice or revealed the preimage.

* Shared asset kits now isolate proof and RPC buffers, reject oversized proofs
  and unencodable amounts, and enforce distinct x-only signing keys across all
  sender and receiver roles. Each party may still reuse its own internal key
  as its script key for legacy contracts.

* Shared legacy asset contracts now freeze address, asset, virtual packet,
  commitment and proof versions in one policy definition. Unknown input proof
  semantics are rejected before commitment reconstruction or verification.

* Shared deposit and HTLC addresses are now checked against their complete
  contracts, with output keys derived from trusted asset metadata. RPC responses
  must agree with their encoded addresses; shared validators also accept peer
  addresses.

* Deposit proof acceptance and refund signing now enforce the expected group
  key when trusted asset metadata is supplied. Proof-only kits without that
  metadata remain supported.

* Shared asset sweep signing now requires Bitcoin locktimes and input sequences
  to enforce asset timelocks, preventing anchors from confirming before their
  asset transitions are valid while preserving existing signatures.

* Shared asset HTLC proof acceptance now binds the proof to the contract's
  HTLC output. `SwapKit.ValidateProof` checks the anchor's internal key,
  tapscript sibling and output script, and `GetPkScriptFromProof` now rejects
  proofs anchored anywhere else, so a funder cannot pass off an output it
  controls alone as the HTLC.

* Shared asset HTLCs can now be accepted from a complete proof file with
  `SwapKit.VerifyProofFile`, which rejects histories without genesis
  provenance, bounds the nesting depth and decoded size of additional inputs,
  and has tapd verify the file. HTLC proof acceptance also enforces the
  Taproot Assets v0.8.5 proof activation rules, rejecting transfers anchored
  from the activation height without the proofs those rules require.

* Shared asset sweep signing now rejects splits whose root asset is not the
  root locator committed in the split tree, as tapd checks before publishing.
  The kit could otherwise sign a sweep whose proofs Taproot Assets v0.8.5
  nodes reject.

* Shared asset sweeps validate asset conservation and Bitcoin output commitments,
  including change, passive assets, and the STXO and spender leaves required by
  Taproot Assets v0.8.5, before signing. Fresh inclusion and exclusion proofs
  check the complete Bitcoin transaction, rejecting extra outputs that would
  invalidate asset proofs.

* Shared asset HTLC helpers and funding templates now return independent NUMS
  keys so caller mutations cannot change later contracts.

* Select Taproot Assets' bounded TLV decoder in the consuming modules so a
  malformed asset proof returns an error before allocating from attacker-supplied
  TLV lengths.

#### Maintenance

* Update Taproot Assets to v0.8.6, taprpc to v1.3.3, and the LND dependency to
  v0.21.3-beta, retaining upstream's Go 1.27.3 build requirement.

- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
