package tapsdk

// Network defines the chain that we operate on.
type Network string

const (
	// NetworkMainnet is bitcoin mainnet.
	NetworkMainnet Network = "mainnet"

	// NetworkTestnet is bitcoin testnet.
	NetworkTestnet Network = "testnet"

	// NetworkTestnet4 is bitcoin testnet version 4.
	NetworkTestnet4 Network = "testnet4"

	// NetworkRegtest is bitcoin regtest.
	NetworkRegtest Network = "regtest"

	// NetworkSimnet is bitcoin simnet.
	NetworkSimnet Network = "simnet"

	// NetworkSignet is bitcoin signet.
	NetworkSignet Network = "signet"
)
