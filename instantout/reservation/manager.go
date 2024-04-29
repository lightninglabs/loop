package reservation

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

	// hasL402 is true if the client has a valid L402.
	hasL402 bool

	runCtx context.Context

	sync.Mutex
}

// NewManager creates a new reservation manager.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:                cfg,
		activeReservations: make(map[ID]*FSM),
	}
}

// Run runs the reservation manager.
func (m *Manager) Run(ctx context.Context, height int32) error {
	log.Debugf("Starting reservation manager")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	m.runCtx = runCtx
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

	err = m.RegisterReservationNotifications(reservationResChan)
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
			_, err := m.newReservation(
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
	req *reservationrpc.ServerReservationNotification) (*FSM, error) {

	var reservationID ID
	err := reservationID.FromByteSlice(
		req.ReservationId,
	)
	if err != nil {
		return nil, err
	}

	serverKey, err := btcec.ParsePubKey(req.ServerKey)
	if err != nil {
		return nil, err
	}

	// Create the reservation state machine. We need to pass in the runCtx
	// of the reservation manager so that the state machine will keep on
	// running even if the grpc conte
	reservationFSM := NewFSM(
		ctx, m.cfg,
	)

	// Add the reservation to the active reservations map.
	m.Lock()
	m.activeReservations[reservationID] = reservationFSM
	m.Unlock()

	initContext := &InitReservationContext{
		reservationID: reservationID,
		serverPubkey:  serverKey,
		value:         btcutil.Amount(req.Value),
		expiry:        req.Expiry,
		heightHint:    currentHeight,
	}

	// Send the init event to the state machine.
	go func() {
		err = reservationFSM.SendEvent(OnServerRequest, initContext)
		if err != nil {
			log.Errorf("Error sending init event: %v", err)
		}
	}()

	// We'll now wait for the reservation to be in the state where it is
	// waiting to be confirmed.
	err = reservationFSM.DefaultObserver.WaitForState(
		ctx, 5*time.Second, WaitForConfirmation,
		fsm.WithWaitForStateOption(time.Second),
	)
	if err != nil {
		if reservationFSM.LastActionError != nil {
			return nil, fmt.Errorf("error waiting for "+
				"state: %v, last action error: %v",
				err, reservationFSM.LastActionError)
		}
		return nil, err
	}

	return reservationFSM, nil
}

// fetchL402 fetches the L402 from the server. This method will keep on
// retrying until it gets a valid response.
func (m *Manager) fetchL402(ctx context.Context) {
	// Add a 0 timer so that we initially fetch the L402 immediately.
	timer := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			err := m.cfg.FetchL402(ctx)
			if err != nil {
				log.Warnf("Error fetching L402: %v", err)
				timer.Reset(time.Second * 10)
				continue
			}
			m.hasL402 = true
			return
		}
	}
}

// RegisterReservationNotifications registers a new reservation notification
// stream.
func (m *Manager) RegisterReservationNotifications(
	reservationChan chan *reservationrpc.ServerReservationNotification) error {

	// In order to create a valid l402 we first are going to call
	// the FetchL402 method. As a client might not have outbound capacity
	// yet, we'll retry until we get a valid response.
	if !m.hasL402 {
		m.fetchL402(m.runCtx)
	}

	ctx, cancel := context.WithCancel(m.runCtx)

	// We'll now subscribe to the reservation notifications.
	reservationStream, err := m.cfg.ReservationClient.
		ReservationNotificationStream(
			ctx, &reservationrpc.ReservationNotificationRequest{},
		)
	if err != nil {
		cancel()
		return err
	}

	log.Debugf("Successfully subscribed to reservation notifications")

	// We'll now start a goroutine that will forward all the reservation
	// notifications to the reservationChan.
	go func() {
		for {
			reservationRes, err := reservationStream.Recv()
			if err == nil && reservationRes != nil {
				log.Debugf("Received reservation %x",
					reservationRes.ReservationId)
				reservationChan <- reservationRes
				continue
			}
			log.Errorf("Error receiving "+
				"reservation: %v", err)

			cancel()

			// If we encounter an error, we'll
			// try to reconnect.
			for {
				select {
				case <-m.runCtx.Done():
					return

				case <-time.After(time.Second * 10):
					log.Debugf("Reconnecting to " +
						"reservation notifications")
					err = m.RegisterReservationNotifications(
						reservationChan,
					)
					if err != nil {
						log.Errorf("Error "+
							"reconnecting: %v", err)
						continue
					}

					// If we were able to reconnect, we'll
					// return.
					return
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

// LockReservation locks the reservation with the given ID.
func (m *Manager) LockReservation(ctx context.Context, id ID) error {
	// Try getting the reservation from the active reservations map.
	m.Lock()
	reservation, ok := m.activeReservations[id]
	m.Unlock()

	if !ok {
		return fmt.Errorf("reservation not found")
	}

	// Try to send the lock event to the reservation.
	err := reservation.SendEvent(OnLocked, nil)
	if err != nil {
		return err
	}

	return nil
}

// UnlockReservation unlocks the reservation with the given ID.
func (m *Manager) UnlockReservation(ctx context.Context, id ID) error {
	// Try getting the reservation from the active reservations map.
	m.Lock()
	reservation, ok := m.activeReservations[id]
	m.Unlock()

	if !ok {
		return fmt.Errorf("reservation not found")
	}

	// Try to send the unlock event to the reservation.
	err := reservation.SendEvent(OnUnlocked, nil)
	if err != nil && strings.Contains(err.Error(), "config error") {
		// If the error is a config error, we can ignore it, as the
		// reservation is already unlocked.
		return nil
	} else if err != nil {
		return err
	}

	return nil
}
