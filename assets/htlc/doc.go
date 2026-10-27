// Package htlc defines the shared, versioned Taproot Asset HTLC construction
// used by Loop and the Loop server.
//
// LegacyDepositV0 preserves the server's existing asset deposit contract. A
// new protocol must add and explicitly select its own policy before it can
// construct a contract, so this compatibility layer does not silently choose
// the on-chain terms of a future Loop Asset Out protocol.
package htlc
