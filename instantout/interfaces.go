package instantout

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/instantout/reservation"
)

const (
	// TODO(sputn1ck): decide on actual value.
	KeyFamily = int32(42069)
)

// InstantLoopOutStore is the interface that needs to be implemented by a
// store that wants to be used by the instant loop out manager.
type InstantLoopOutStore interface {
	// CreateInstantLoopOut adds a new instant loop out to the store.
	CreateInstantLoopOut(ctx context.Context, instantOut *InstantOut) error

	// UpdateInstantLoopOut updates an existing instant loop out in the
	// store.
	UpdateInstantLoopOut(ctx context.Context, instantOut *InstantOut) error

	// GetInstantLoopOut returns the instant loop out for the given swap
	// hash.
	GetInstantLoopOut(ctx context.Context,
		swapHash []byte) (*InstantOut, error)

	// ListInstantLoopOuts returns all instant loop outs that are in the
	// store.
	ListInstantLoopOuts(ctx context.Context) ([]*InstantOut, error)
}

// ReservationManager handles fetching and locking of reservations.
type ReservationManager interface {
	// GetReservation returns the reservation for the given id.
	GetReservation(ctx context.Context, id reservation.ID) (
		*reservation.Reservation, error)

	// LockReservation locks the reservation for the given id.
	LockReservation(ctx context.Context, id reservation.ID) error

	// UnlockReservation unlocks the reservation for the given id.
	UnlockReservation(ctx context.Context, id reservation.ID) error
}

// InputReservations is a helper struct for the input reservations.
type InputReservations []InputReservation

// InputReservation is a helper struct for the input reservation.
type InputReservation struct {
	Outpoint wire.OutPoint
	Value    btcutil.Amount
	PkScript []byte
}

// Output returns the output for the input reservation.
func (r InputReservation) Output() *wire.TxOut {
	return wire.NewTxOut(int64(r.Value), r.PkScript)
}

// GetPrevoutFetcher returns a prevout fetcher for the input reservations.
func (i InputReservations) GetPrevoutFetcher() txscript.PrevOutputFetcher {
	prevOuts := make(map[wire.OutPoint]*wire.TxOut)

	// add the reservation inputs
	for _, reservation := range i {
		prevOuts[reservation.Outpoint] = reservation.Output()
	}

	return txscript.NewMultiPrevOutFetcher(prevOuts)
}
