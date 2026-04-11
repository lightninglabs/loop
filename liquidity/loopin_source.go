package liquidity

import "fmt"

// LoopInSource identifies the funding source that autoloop should use for loop
// in swaps.
type LoopInSource uint8

const (
	// LoopInSourceWallet uses the legacy wallet-funded loop-in flow.
	LoopInSourceWallet LoopInSource = iota

	// LoopInSourceStaticAddress uses deposited static-address funds for
	// loop-ins and does not fall back to wallet-funded loop-ins.
	LoopInSourceStaticAddress
)

// String returns a human-readable representation of the source.
func (s LoopInSource) String() string {
	switch s {
	case LoopInSourceWallet:
		return "wallet"

	case LoopInSourceStaticAddress:
		return "static-address"

	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}
