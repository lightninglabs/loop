package loopdb

import (
	"context"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
)

// SwapStore is the primary database interface used by the loopd system. It
// houses information for all pending completed/failed swaps.
type SwapStore interface {
	// FetchLoopOutSwaps returns all swaps currently in the store.
	FetchLoopOutSwaps(ctx context.Context) ([]*LoopOut, error)

	// FetchLoopOutSwap returns the loop out swap with the given hash.
	FetchLoopOutSwap(ctx context.Context, hash lntypes.Hash) (*LoopOut, error)

	// CreateLoopOut adds an initiated swap to the store.
	CreateLoopOut(ctx context.Context, hash lntypes.Hash,
		swap *LoopOutContract) error

	// BatchCreateLoopOut creates a batch of loop out swaps to the store.
	BatchCreateLoopOut(ctx context.Context,
		swaps map[lntypes.Hash]*LoopOutContract) error

	// UpdateLoopOut stores a new event for a target loop out swap. This
	// appends to the event log for a particular swap as it goes through
	// the various stages in its lifetime.
	UpdateLoopOut(ctx context.Context, hash lntypes.Hash, time time.Time,
		state SwapStateData) error

	// FetchLoopInSwaps returns all swaps currently in the store.
	FetchLoopInSwaps(ctx context.Context) ([]*LoopIn, error)

	// CreateLoopIn adds an initiated swap to the store.
	CreateLoopIn(ctx context.Context, hash lntypes.Hash,
		swap *LoopInContract) error

	// BatchCreateLoopIn creates a batch of loop in swaps to the store.
	BatchCreateLoopIn(ctx context.Context,
		swaps map[lntypes.Hash]*LoopInContract) error

	// UpdateLoopIn stores a new event for a target loop in swap. This
	// appends to the event log for a particular swap as it goes through
	// the various stages in its lifetime.
	UpdateLoopIn(ctx context.Context, hash lntypes.Hash, time time.Time,
		state SwapStateData) error

	// BatchInsertUpdate inserts batch of swap updates to the store.
	BatchInsertUpdate(ctx context.Context,
		updateData map[lntypes.Hash][]BatchInsertUpdateData) error

	// PutLiquidityParams writes the serialized `manager.Parameters` bytes
	// into the bucket.
	//
	// NOTE: it's the caller's responsibility to encode the param. Atm,
	// it's encoding using the proto package's `Marshal` method.
	PutLiquidityParams(ctx context.Context, params []byte) error

	// FetchLiquidityParams reads the serialized `manager.Parameters` bytes
	// from the bucket.
	//
	// NOTE: it's the caller's responsibility to decode the param. Atm,
	// it's decoding using the proto package's `Unmarshal` method.
	FetchLiquidityParams(ctx context.Context) ([]byte, error)

	// Close closes the underlying database.
	Close() error
}

// BatchInsertUpdateData is a struct that holds the data for the
// BatchInsertUpdate function.
type BatchInsertUpdateData struct {
	Time  time.Time
	State SwapStateData
}

// TODO(roasbeef): back up method in interface?
