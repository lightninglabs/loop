// Package deposit contains the legacy Taproot Asset deposit contract used by
// the Loop integration environment. It owns deposit address construction,
// proof validation, and the unilateral timeout path. Asset HTLC construction
// remains in the sibling htlc package.
// Constructor parameters preserve separate sender/receiver internal and script
// keys, using the on-chain asset direction for both deposit and HTLC contracts.
// Derived HTLCs explicitly select htlc.AssetHTLCV0 in either swap direction;
// its htlc.LegacyDepositV0 alias preserves compatibility with existing drafts.
// Different HTLC terms require a new policy rather than a change to this kit's
// derived contract.
// Address creation and peer-address validation additionally require trusted
// genesis metadata and the expected group key, so output keys can be derived
// independently of an RPC response. Supplied group metadata also constrains
// proof acceptance and refunds. Proof-only operations need no such metadata.
//
// NewAddr, NewHtlcAddr and VerifyProofFile check the running tapd against the
// shared on-chain swap version floor. Full-history acceptance during recovery
// also needs a compliant verifier. Local refund signing stays available with
// verified proofs, but the caller must construct compliant new transfers and
// manage recovery. New swap offers and funding outside this kit need the same
// version check before committing funds.
//
// Consuming main Go modules must select the bounded TLV decoder themselves:
// replace github.com/lightningnetwork/lnd/tlv with
// github.com/lightninglabs/taproot-assets/third_party/lnd/tlv at
// v0.0.0-20261016193543-734ce74dd1e8, or use
// github.com/lightningnetwork/lnd/tlv v1.4.1 with a compatible newer stack.
// Replacements in dependencies do not propagate to consumers. The tapd
// runtime check does not validate the local proof decoder.
package deposit
