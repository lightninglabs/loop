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

* `SwapKit.CreateHtlcVpkt` now returns a non-interactive funding template that
  matches V1 HTLC addresses for exact-value and change sends. Integrations
  using the original interactive template must explicitly call
  `CreateInteractiveHtlcVpkt` and validate the actual anchor proof.

* Shared asset HTLC witness builders now require complete, caller-approved
  virtual transfers. Construct transfers with `sweep.NewTransfer` to snapshot
  the approved Bitcoin output values, scripts and order before signing.

#### Bug Fixes

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
