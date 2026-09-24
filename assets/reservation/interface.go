package reservation

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightninglabs/loop/fsm"
)

// StateFilter selects reservations by their latest saved state. State and
// ActiveOnly are mutually exclusive; neither means all states. ActiveOnly
// excludes QuoteFailed, QuoteRejected, Canceled and Expired, retaining Ready
// and NeedAdminAttention.
type StateFilter struct {
	State      fsm.StateType
	ActiveOnly bool
}

// Validate rejects conflicting filters and unknown state names.
func (f StateFilter) Validate() error {
	if f.ActiveOnly && f.State != "" {
		return errors.New("state and active_only are mutually exclusive")
	}
	switch f.State {
	case "", RequestQuote, ProbeRoutes, AwaitApproval, PayPrepay,
		WaitForDelivery, VerifyReservation, Ready, CancelPrepay,
		QuoteFailed, QuoteRejected, Canceled, Expired, NeedAdminAttention:

	default:
		return fmt.Errorf("unknown reservation state %q", f.State)
	}
	return nil
}

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

	// UpdateReservation saves changed purchase facts and a state history entry
	// atomically. It accepts the first quote, then preserves agreed terms and
	// established payment and delivery facts. It returns ErrNotFound for an
	// unknown ID or ErrRequestConflict for changes to protected values.
	// Unchanged progress adds no history entry. On success, the supplied
	// reservation reflects the saved values, including its timestamps.
	UpdateReservation(context.Context, *Reservation) error

	// GetReservation loads a validated snapshot of the reservation's terms,
	// purchase facts, and latest state in one transaction. It returns
	// ErrNotFound if the ID is absent. Changing the returned value does not
	// change storage until UpdateReservation succeeds.
	GetReservation(context.Context, ID) (*Reservation, error)

	// GetReservations reads matching records and their latest state together
	// in one statement. Invalid matching data fails the read rather than
	// silently omitting a purchase.
	GetReservations(context.Context, StateFilter) ([]*Reservation, error)
}
