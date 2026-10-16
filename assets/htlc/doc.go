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
// Callers must explicitly select a policy and retain its terms for the life
// of the swap. A new direction or application can reuse identical terms;
// changes to scripts, key derivation, timelocks or supported wire versions
// require a distinct policy. A policy must never be redefined to migrate an
// existing contract to new terms.
//
// AssetHTLCV0 retains success CSV=1 and the shared OP_TRUE asset script key.
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
package htlc
