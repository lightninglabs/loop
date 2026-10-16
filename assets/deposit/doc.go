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
package deposit
