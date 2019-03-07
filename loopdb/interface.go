package loopdb

import (
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
)

// SwapStore is the priamry database interface used by the loopd system. It
// houses informatino for all pending completed/failed swaps.
type SwapStore interface {
	// FetchUnchargeSwaps returns all swaps currently in the store.
	FetchUnchargeSwaps() ([]*PersistentUncharge, error)

	// CreateUncharge adds an initiated swap to the store.
	CreateUncharge(hash lntypes.Hash, swap *UnchargeContract) error

	// UpdateUncharge stores a swap updateUncharge. This appends to the
	// event log for a particular swap as it goes through the various
	// stages in its lifetime.
	UpdateUncharge(hash lntypes.Hash, time time.Time, state SwapState) error

	// Close closes the underlying database.
	Close() error
}

// TODO(roasbeef): back up method in interface?
