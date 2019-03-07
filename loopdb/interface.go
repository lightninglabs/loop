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

	// CreateLoopOut adds an initiated swap to the store.
	CreateLoopOut(hash lntypes.Hash, swap *LoopOutContract) error

	// UpdateLoopOut stores a new event for a target loop out swap. This
	// appends to the event log for a particular swap as it goes through
	// the various stages in its lifetime.
	UpdateLoopOut(hash lntypes.Hash, time time.Time, state SwapState) error

	// Close closes the underlying database.
	Close() error
}

// TODO(roasbeef): back up method in interface?
