package reservation

import (
	"context"
	"errors"
)

var (
	// ErrNotFound means the requested reservation is unavailable.
	ErrNotFound = errors.New("asset reservation not found")

	// ErrRequestConflict means an ID was reused with different terms or keys.
	ErrRequestConflict = errors.New("asset reservation request conflict")
)

// Store persists reservations and their state history. The manager serializes
// updates to each reservation; the store never changes agreed terms.
type Store interface {
	// CreateReservation saves a reservation and its initial state history
	// atomically. Repeating a matching request loads its saved progress into
	// the supplied reservation; a conflicting request returns ErrRequestConflict.
	// On success, the supplied reservation includes its stored timestamps.
	CreateReservation(context.Context, *Reservation) error

	// UpdateReservation saves progress and a state history entry atomically.
	// It preserves agreed terms and client keys, returning ErrRequestConflict
	// if they change, or ErrNotFound if the ID is absent. On success, the
	// supplied reservation includes its stored timestamps.
	UpdateReservation(context.Context, *Reservation) error

	// GetReservation loads a validated snapshot of the reservation's terms
	// and latest state in one transaction. It returns
	// ErrNotFound if the ID is absent. Changing the returned value does not
	// change storage until UpdateReservation succeeds.
	GetReservation(context.Context, ID) (*Reservation, error)

	// GetReservations loads validated snapshots of all saved reservations in
	// one transaction, including canceled and expired purchases. The manager
	// selects which ones need recovery. Invalid stored data fails the whole
	// read so recovery cannot silently omit a reservation.
	GetReservations(context.Context) ([]*Reservation, error)
}
