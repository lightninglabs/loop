// Package htlc defines the shared, versioned Taproot Asset HTLC construction
// used by Loop and the Loop server.
//
// AssetHTLCV0 preserves the existing asset deposit HTLC contract and is the
// shared policy for Asset Loop In and Asset Loop Out using those terms. Sender
// and receiver follow the on-chain asset direction: the client sends assets
// to the server for Loop In, and the server sends assets to the client for
// Loop Out. Internal keys form the cooperative MuSig2 aggregate; the sender
// script key authorizes refunds and the receiver script key authorizes
// preimage claims. Supplying each party's original key for both roles
// preserves the legacy contract vectors.
//
// LegacyDepositV0 remains an alias for AssetHTLCV0 with its historical numeric
// value of 1. Existing drafts can retain that name and their negotiated inputs
// without changing scripts or keys. The original serialized interactive
// funding template remains available through CreateInteractiveHtlcVpkt.
// V0 denotes the Loop contract revision, independently of Taproot Assets
// wire versions.
//
// AssetHTLCCLTVV1 selects the static Loop In V3 Bitcoin script layout with an
// absolute refund height. Construct it with Params.RefundHeight set and
// Params.CsvExpiry zero. The success leaf still requires CSV=1. Both policies
// support either direction and retain the same cooperative internal-key
// aggregation, four key roles, asset script key and witness stack order.
// The CLTV policy does not reproduce a caller's custom asset script key.
//
// SuccessSpendConstraints and TimeoutSpendConstraints expose the Bitcoin
// transaction requirements without depending on a signing backend. Before
// signing any inputs, use AssetInputIndex to identify the proof-bound input
// and set its sequence and the transaction's locktime. CLTV refunds use
// sequence 0xfffffffe (non-final with BIP68 disabled) and nLockTime at least
// RefundHeight, strictly below the timestamp threshold. Witness builders
// validate these terms without mutating the transaction or fee signatures.
// Existing leaf, control block and output-script methods also support both
// policies, allowing other signing backends to use the same contract.
//
// The caller must negotiate and persist the exact policy and terms before
// funding, verify the funding history and current unspent outpoint, and
// enforce confirmation depth, chain finality and Lightning deadline margins.
// CLTV's clock does not restart when a deposit is converted to an HTLC; the
// flow must reject funding when too little refund time remains. Bitcoin
// finality requires a block height greater than nLockTime. Deposit CSV expiry
// and the HTLC's absolute refund height are independent. This kit does not
// implement invoice settlement, cancellation or swap state machines.
//
// Callers must explicitly select a policy and retain its terms for the life
// of the swap. A new direction or application can reuse identical terms;
// changes to scripts, key derivation, timelocks or supported wire versions
// require a distinct policy. A policy must never be redefined to migrate an
// existing contract to new terms.
//
// Both policies retain success CSV=1 and the shared OP_TRUE asset script key.
// Multiple HTLC outputs for the same asset using that shared key cannot be
// batched in one transaction, and proof delivery needs a discriminator beyond
// that shared script key.
// Contract-specific asset script keys would require a new policy.
//
// CreateHtlcVpkt uses non-interactive funding and matches the predictive V1
// contract address for exact-value and change sends. Existing integrations
// that negotiate interactive funding must explicitly select
// CreateInteractiveHtlcVpkt and validate the actual proof-derived output key.
// Sharing a policy does not make the two funding modes interchangeable.
//
// A receiver accepts a counterparty's HTLC with SwapKit.VerifyProofFile, which
// has tapd verify the complete history, or with SwapKit.ValidateProof for a
// proof whose history is already verified. Both bind the proof to the
// contract's HTLC output and apply the Taproot Assets v0.8.5 proof activation
// rules. Commitment proofs alone show where an asset is anchored, not that
// this contract's HTLC controls it.
//
// VerifyProofFile checks the running tapd and requires the shared on-chain
// swap version floor, including when re-verifying an existing contract.
// Pure contract and claim/refund witness construction stay available for
// recovery with verified proofs. Callers must gate new swap offers and funding
// outside this kit, and recovery must still construct transfers compliant with
// the network activation rules. This package supplies no recovery state
// machine.
//
// Consuming main Go modules must select the bounded TLV decoder themselves:
// replace github.com/lightningnetwork/lnd/tlv with
// github.com/lightninglabs/taproot-assets/third_party/lnd/tlv at
// v0.0.0-20261016193543-734ce74dd1e8, or use
// github.com/lightningnetwork/lnd/tlv v1.4.1 with a compatible newer stack.
// Replacements in dependencies do not propagate to consumers. The tapd
// runtime check does not validate the local proof decoder.
package htlc
