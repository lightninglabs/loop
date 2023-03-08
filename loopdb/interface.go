package loopdb

import (
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
)

// SwapStore is the primary database interface used by the loopd system. It
// houses information for all pending completed/failed swaps.
type SwapStore interface {
	// FetchLoopOutSwaps returns all swaps currently in the store.
	FetchLoopOutSwaps() ([]*LoopOut, error)

	// FetchLoopOutSwap returns the loop out swap with the given hash.
	FetchLoopOutSwap(hash lntypes.Hash) (*LoopOut, error)

	// CreateLoopOut adds an initiated swap to the store.
	CreateLoopOut(hash lntypes.Hash, swap *LoopOutContract) error

	// UpdateLoopOut stores a new event for a target loop out swap. This
	// appends to the event log for a particular swap as it goes through
	// the various stages in its lifetime.
	UpdateLoopOut(hash lntypes.Hash, time time.Time,
		state SwapStateData) error

	// FetchLoopInSwaps returns all swaps currently in the store.
	FetchLoopInSwaps() ([]*LoopIn, error)

	// CreateLoopIn adds an initiated swap to the store.
	CreateLoopIn(hash lntypes.Hash, swap *LoopInContract) error

	// UpdateLoopIn stores a new event for a target loop in swap. This
	// appends to the event log for a particular swap as it goes through
	// the various stages in its lifetime.
	UpdateLoopIn(hash lntypes.Hash, time time.Time,
		state SwapStateData) error

	// PutLiquidityParams writes the serialized `manager.Parameters` bytes
	// into the bucket.
	//
	// NOTE: it's the caller's responsibility to encode the param. Atm,
	// it's encoding using the proto package's `Marshal` method.
	PutLiquidityParams(params []byte) error

	// FetchLiquidityParams reads the serialized `manager.Parameters` bytes
	// from the bucket.
	//
	// NOTE: it's the caller's responsibility to decode the param. Atm,
	// it's decoding using the proto package's `Unmarshal` method.
	FetchLiquidityParams() ([]byte, error)

	// Close closes the underlying database.
	Close() error
}

// TODO(roasbeef): back up method in interface?
