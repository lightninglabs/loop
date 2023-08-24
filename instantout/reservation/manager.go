package reservation

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"

	"github.com/lightninglabs/loop/fsm"
	reservationrpc "github.com/lightninglabs/loop/swapserverrpc"
)

// Manager manages the reservation state machines.
type Manager struct {
	// cfg contains all the services that the reservation manager needs to
	// operate.
	cfg *Config

	// activeReservations contains all the active reservationsFSMs.
	activeReservations map[ID]*FSM
}

// NewReservationManager creates a new reservation manager.
func NewReservationManager(cfg *Config) *Manager {
	return &Manager{
		cfg:                cfg,
		activeReservations: make(map[ID]*FSM),
	}
}

// Run runs the reservation manager.
func (m *Manager) Run(ctx context.Context, height int32) error {
	// todo(sputn1ck): recover swaps on startup
	log.Debugf("Starting reservation manager")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	currentHeight := height

	err := m.RecoverReservations(runCtx)
	if err != nil {
		return err
	}

	newBlockChan, newBlockErrChan, err := m.cfg.ChainNotifier.
		RegisterBlockEpochNtfn(runCtx)
	if err != nil {
		return err
	}

	reservationResChan := make(
		chan *reservationrpc.ServerReservationNotification,
	)

	err = m.RegisterReservationNotifications(runCtx, reservationResChan)
	if err != nil {
		return err
	}

	for {
		select {
		case height := <-newBlockChan:
			log.Debugf("Received block %v", height)
			currentHeight = height

		case reservationRes := <-reservationResChan:
			log.Debugf("Received reservation %x",
				reservationRes.ReservationId)
			err := m.newReservation(
				runCtx, uint32(currentHeight), reservationRes,
			)
			if err != nil {
				return err
			}

		case err := <-newBlockErrChan:
			return err

		case <-runCtx.Done():
			log.Debugf("Stopping reservation manager")
			return nil
		}
	}
}

// newReservation creates a new reservation from the reservation request.
func (m *Manager) newReservation(ctx context.Context, currentHeight uint32,
	req *reservationrpc.ServerReservationNotification) error {

	var reservationID ID
	err := reservationID.FromByteSlice(
		req.ReservationId,
	)
	if err != nil {
		return err
	}

	serverKey, err := btcec.ParsePubKey(req.ServerKey)
	if err != nil {
		return err
	}

	// Create the reservation state machine. We need to pass in the runCtx
	// of the reservation manager so that the state machine will keep on
	// running even if the grpc conte
	reservationFSM := NewFSM(
		ctx, m.cfg,
	)

	initContext := &InitReservationContext{
		reservationID: reservationID,
		serverPubkey:  serverKey,
		value:         btcutil.Amount(req.Value),
		expiry:        req.Expiry,
		heightHint:    currentHeight,
	}

	// Send the init event to the state machine.
	err = reservationFSM.SendEvent(OnServerRequest, initContext)
	if err != nil {
		return err
	}

	// We'll now wait for the reservation to be in the state where it is
	// waiting to be confirmed.
	err = reservationFSM.DefaultObserver.WaitForState(
		ctx, time.Minute, WaitForConfirmation,
		fsm.WithWaitForStateOption(time.Second),
	)
	if err != nil {
		return err
	}

	return nil
}

// RegisterReservationNotifications registers a new reservation notification
// stream.
func (m *Manager) RegisterReservationNotifications(
	ctx context.Context, reservationChan chan *reservationrpc.
		ServerReservationNotification) error {

	// In order to create a valid lsat we first are going to call
	// the FetchL402 method.
	err := m.cfg.FetchL402(ctx)
	if err != nil {
		return err
	}

	// We'll now subscribe to the reservation notifications.
	reservationStream, err := m.cfg.ReservationClient.
		ReservationNotificationStream(
			ctx, &reservationrpc.ReservationNotificationRequest{},
		)
	if err != nil {
		return err
	}

	// We'll now start a goroutine that will forward all the reservation
	// notifications to the reservationChan.
	go func() {
		for {
			reservationRes, err := reservationStream.Recv()
			if err == nil && reservationRes != nil {
				reservationChan <- reservationRes
				continue
			}
			log.Errorf("Error receiving "+
				"reservation: %v", err)

			reconnectTimer := time.NewTimer(time.Second * 10)

			// If we encounter an error, we'll
			// try to reconnect.
			for {
				select {
				case <-ctx.Done():
					return
				case <-reconnectTimer.C:
					err = m.RegisterReservationNotifications(
						ctx, reservationChan,
					)
					if err == nil {
						log.Debugf(
							"Successfully " +
								"reconnected",
						)
						reconnectTimer.Stop()
						// If we were able to
						// reconnect, we'll
						// return.
						return
					}
					log.Errorf("Error "+
						"reconnecting: %v",
						err)

					reconnectTimer.Reset(
						time.Second * 10,
					)
				}
			}
		}
	}()

	return nil
}

// RecoverReservations tries to recover all reservations that are still active
// from the database.
func (m *Manager) RecoverReservations(ctx context.Context) error {
	reservations, err := m.cfg.Store.ListReservations(ctx)
	if err != nil {
		return err
	}

	for _, reservation := range reservations {
		if isFinalState(reservation.State) {
			continue
		}

		log.Debugf("Recovering reservation %x", reservation.ID)

		fsmCtx := context.WithValue(ctx, reservation.ID, nil)

		reservationFSM := NewFSMFromReservation(
			fsmCtx, m.cfg, reservation,
		)

		m.activeReservations[reservation.ID] = reservationFSM

		// As SendEvent can block, we'll start a goroutine to process
		// the event.
		go func() {
			err := reservationFSM.SendEvent(OnRecover, nil)
			if err != nil {
				log.Errorf("FSM %v Error sending recover "+
					"event %v, state: %v",
					reservationFSM.reservation.ID, err,
					reservationFSM.reservation.State)
			}
		}()
	}

	return nil
}

// GetReservations retrieves all reservations from the database.
func (m *Manager) GetReservations(ctx context.Context) ([]*Reservation, error) {
	return m.cfg.Store.ListReservations(ctx)
}

// GetReservation returns the reservation for the given id.
func (m *Manager) GetReservation(ctx context.Context, id ID) (*Reservation,
	error) {

	return m.cfg.Store.GetReservation(ctx, id)
}

// LockReservation locks the reservation for the given id.
func (m *Manager) LockReservation(ctx context.Context, id ID) error {
	// TODO(sputn1ck): implement
	return nil
}

// UnlockReservation unlocks the reservation for the given id.
func (m *Manager) UnlockReservation(ctx context.Context, id ID) error {
	// TODO(sputn1ck): implement
	return nil
}
