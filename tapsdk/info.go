package tapsdk

import "github.com/btcsuite/btcd/chaincfg/chainhash"

// Info contains info about the connected tapd instance.
type Info struct {
	// Version is the version that tapd is running.
	Version string

	// LndVersion is the full version string of the LND node tapd is connected to.
	LndVersion string

	// Network is the network tapd is connected to,
	// e.g. "mainnet", "testnet", or any other supported network.
	Network string

	// LndIdentityPubkey is the public key of the LND node tapd is connected to.
	LndIdentityPubkey PubKey

	// NodeAlias is the alias of the LND node tapd is connected to.
	NodeAlias string

	// BlockHeight is the best block height that tapd has knowledge of.
	BlockHeight uint32

	// BlockHash is the current block hash as seen by the LND node
	// tapd is connected to.
	BlockHash chainhash.Hash

	// SyncedToChain is true if the wallet's view is synced to the main
	// chain.
	SyncedToChain bool
}
