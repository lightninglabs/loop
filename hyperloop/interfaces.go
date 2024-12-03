package hyperloop

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
)

// Store is the interface that stores the hyperloop.
type Store interface {
	// CreateHyperloop stores the hyperloop in the database.
	CreateHyperloop(ctx context.Context, hyperloop *Hyperloop) error

	// UpdateHyperloop updates the hyperloop in the database.
	UpdateHyperloop(ctx context.Context, hyperloop *Hyperloop) error

	// CreateHyperloopParticipant stores the hyperloop participant in the
	// database.
	CreateHyperloopParticipant(ctx context.Context,
		participant *HyperloopParticipant) error

	// UpdateHyperloopParticipant updates the hyperloop participant in the
	// database.
	UpdateHyperloopParticipant(ctx context.Context,
		participant *HyperloopParticipant) error

	// GetHyperloop retrieves the hyperloop from the database.
	GetHyperloop(ctx context.Context, id ID) (*Hyperloop, error)

	// ListHyperloops lists all existing hyperloops the client has ever
	// made.
	ListHyperloops(ctx context.Context) ([]*Hyperloop, error)
}

type HyperloopManager interface {
	fetchHyperLoopTotalSweepAmt(hyperloopID ID,
		sweepAddr btcutil.Address) (btcutil.Amount, error)
}
