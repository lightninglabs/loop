package reservation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/fsm"
	reservationrpc "github.com/lightninglabs/loop/swapserverrpc"
)

var reservationStateWaitTimeout = 5 * time.Second

// Manager manages the reservation state machines.
type Manager struct {
	sync.Mutex

	// cfg contains all the services that the reservation manager needs to
	// operate.
	cfg *Config

	// activeReservations contains all the active reservationsFSMs.
	activeReservations map[ID]*FSM
}

// finalStateObserver removes a reservation FSM from the active set once it
// reaches a terminal state.
type finalStateObserver struct {
	manager *Manager
	id      ID
	fsm     *FSM
}

// reservationInitObserver records when a new reservation has completed its
// initialization. It is registered before the FSM starts so a fast funding
// confirmation cannot make the manager miss the intermediate state.
type reservationInitObserver struct {
	reached chan struct{}
	once    sync.Once
}

// Notify implements the fsm.Observer interface.
func (o *reservationInitObserver) Notify(notification fsm.Notification) {
	if notification.NextState != WaitForConfirmation {
		return
	}

	o.once.Do(func() {
		close(o.reached)
	})
}

// Notify implements the fsm.Observer interface.
func (o *finalStateObserver) Notify(notification fsm.Notification) {
	if !isFinalState(notification.NextState) {
		return
	}

	o.manager.Lock()
	defer o.manager.Unlock()

	if o.manager.activeReservations[o.id] == o.fsm {
		delete(o.manager.activeReservations, o.id)
	}
}

// NewManager creates a new reservation manager.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:                cfg,
		activeReservations: make(map[ID]*FSM),
	}
}

// Run runs the reservation manager.
func (m *Manager) Run(ctx context.Context, height int32,
	initChan chan struct{}) error {

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

	ntfnChan := m.cfg.NotificationManager.SubscribeReservations(runCtx)

	// Signal that the manager has been initialized.
	close(initChan)

	for {
		select {
		case height := <-newBlockChan:
			log.Debugf("Received block %v", height)
			currentHeight = height

		case reservationRes, ok := <-ntfnChan:
			if !ok {
				// The channel has been closed, we'll stop the
				// reservation manager.
				log.Debugf("Stopping reservation manager (ntfnChan closed)")
				return nil
			}

			log.Debugf("Received reservation %x",
				reservationRes.ReservationId)
			_, err := m.newReservation(
				runCtx, uint32(currentHeight), reservationRes,
			)
			if err != nil {
				log.Errorf("Unable to create reservation %x: %v",
					reservationRes.ReservationId, err)
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

	_, err = m.cfg.Store.GetReservation(ctx, reservationID)
	switch {
	case err == nil:
		return nil, ErrReservationAlreadyExists

	case !errors.Is(err, ErrReservationNotFound):
		return nil, err
	}

	// Create the reservation state machine. We need to pass in the runCtx
	// of the reservation manager so that the state machine will keep on
	// running even if the grpc conte
	reservationFSM := NewFSM(m.cfg)

	// Add the reservation to the active reservations map. Check the map while
	// holding the lock as concurrent callers may both have completed the store
	// lookup above.
	m.Lock()
	if _, ok := m.activeReservations[reservationID]; ok {
		m.Unlock()
		return nil, ErrReservationAlreadyExists
	}
	if len(m.activeReservations) >= maxActiveReservations {
		m.Unlock()
		return nil, ErrTooManyActiveReservations
	}
	m.activeReservations[reservationID] = reservationFSM
	m.Unlock()

	reservationFSM.RegisterObserver(&finalStateObserver{
		manager: m,
		id:      reservationID,
		fsm:     reservationFSM,
	})
	initObserver := &reservationInitObserver{
		reached: make(chan struct{}),
	}
	reservationFSM.RegisterObserver(initObserver)
	defer reservationFSM.RemoveObserver(initObserver)

	initContext := &InitReservationContext{
		reservationID: reservationID,
		serverPubkey:  serverKey,
		value:         btcutil.Amount(req.Value),
		expiry:        req.Expiry,
		heightHint:    currentHeight,
	}

	// Send the init event to the state machine.
	go func() {
		sendErr := reservationFSM.SendEvent(
			ctx, OnServerRequest, initContext,
		)
		if sendErr != nil {
			log.Errorf("Error sending init event: %v", sendErr)
		}
	}()

	// Wait until initialization reaches the confirmation monitor. The
	// observer was registered before the initialization event, so this also
	// succeeds if the reservation confirms before this select starts.
	timeout := time.NewTimer(reservationStateWaitTimeout)
	defer timeout.Stop()
	var waitErr error
	select {
	case <-initObserver.reached:

	case <-ctx.Done():
		waitErr = ctx.Err()

	case <-timeout.C:
		waitErr = fsm.NewErrWaitingForStateTimeout(WaitForConfirmation)
	}
	if waitErr != nil {
		if reservationFSM.LastActionError != nil {
			return nil, fmt.Errorf("error waiting for "+
				"state: %v, last action error: %v",
				waitErr, reservationFSM.LastActionError)
		}
		return nil, waitErr
	}

	return reservationFSM, nil
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

		reservationFSM := NewFSMFromReservation(m.cfg, reservation)

		m.Lock()
		m.activeReservations[reservation.ID] = reservationFSM
		m.Unlock()
		reservationFSM.RegisterObserver(&finalStateObserver{
			manager: m,
			id:      reservation.ID,
			fsm:     reservationFSM,
		})

		// As SendEvent can block, we'll start a goroutine to process
		// the event.
		go func() {
			err := reservationFSM.SendEvent(fsmCtx, OnRecover, nil)
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
		return ErrReservationNotFound
	}

	// Try to send the lock event to the reservation.
	err := reservation.SendEvent(ctx, OnLocked, nil)
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
		storedReservation, err := m.cfg.Store.GetReservation(ctx, id)
		if err != nil {
			return err
		}

		// Terminal reservations are removed from the active set. Treat an
		// unlock after that removal as idempotent, while still surfacing a
		// missing active FSM for reservations that should be running.
		if isFinalState(storedReservation.State) {
			return nil
		}

		return fmt.Errorf("%w: reservation %x is in state %v",
			ErrReservationNotFound, id, storedReservation.State)
	}

	// Try to send the unlock event to the reservation.
	err := reservation.SendEvent(ctx, OnUnlocked, nil)
	if err != nil && strings.Contains(err.Error(), "config error") {
		// If the error is a config error, we can ignore it, as the
		// reservation is already unlocked.
		return nil
	} else if err != nil {
		return err
	}

	return nil
}
