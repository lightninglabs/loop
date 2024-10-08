package reservation

import (
	"context"
	"fmt"

	"github.com/lightninglabs/loop/swapserverrpc"
)

var (
	ErrReservationAlreadyExists = fmt.Errorf("reservation already exists")
	ErrReservationNotFound      = fmt.Errorf("reservation not found")
)

const (
	KeyFamily         = int32(42068)
	DefaultConfTarget = int32(3)
	IdLength          = 32
)

// Store is the interface that stores the reservations.
type Store interface {
	// CreateReservation stores the reservation in the database.
	CreateReservation(ctx context.Context, reservation *Reservation) error

	// UpdateReservation updates the reservation in the database.
	UpdateReservation(ctx context.Context, reservation *Reservation) error

	// GetReservation retrieves the reservation from the database.
	GetReservation(ctx context.Context, id ID) (*Reservation, error)

	// ListReservations lists all existing reservations the client has ever
	// made.
	ListReservations(ctx context.Context) ([]*Reservation, error)
}

// NotificationManager handles subscribing to incoming reservation
// subscriptions.
type NotificationManager interface {
	SubscribeReservations(context.Context,
	) <-chan *swapserverrpc.ServerReservationNotification
}
