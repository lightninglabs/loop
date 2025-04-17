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
	"github.com/lightninglabs/loop/swapserverrpc"
	reservationrpc "github.com/lightninglabs/loop/swapserverrpc"
)

var (
	defaultWaitForStateTime = time.Second * 15
)

// FSMSendEventReq contains the information needed to send an event to the FSM.
type FSMSendEventReq struct {
	fsm      *FSM
	event    fsm.EventType
	eventCtx fsm.EventContext
}

// Manager manages the reservation state machines.
type Manager struct {
	// cfg contains all the services that the reservation manager needs to
	// operate.
	cfg *Config

	// activeReservations contains all the active reservationsFSMs.
	activeReservations map[ID]*FSM

	currentHeight int32

	reqChan chan *FSMSendEventReq

	sync.Mutex
}

// NewManager creates a new reservation manager.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:                cfg,
		activeReservations: make(map[ID]*FSM),
		reqChan:            make(chan *FSMSendEventReq),
	}
}

// Run runs the reservation manager.
func (m *Manager) Run(ctx context.Context, height int32,
	initChan chan struct{}) error {

	log.Debugf("Starting reservation manager")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	m.currentHeight = height

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
			m.Lock()
			m.currentHeight = height
			m.Unlock()

		case reservationRes, ok := <-ntfnChan:
			if !ok {
				// The channel has been closed, we'll stop the
				// reservation manager.
				log.Debugf("Stopping reservation manager (ntfnChan closed)")
				return nil
			}

			log.Debugf("Received reservation %x",
				reservationRes.ReservationId)
			_, err := m.newReservationFromNtfn(
				runCtx, uint32(m.currentHeight), reservationRes,
			)
			if err != nil {
				return err
			}

		case req := <-m.reqChan:
			// We'll send the event in a goroutine to avoid blocking
			// the main loop.
			go func() {
				err := req.fsm.SendEvent(
					runCtx, req.event, req.eventCtx,
				)
				if err != nil {
					log.Errorf("Error sending event: %v",
						err)
				}
			}()

		case err := <-newBlockErrChan:
			return err

		case <-runCtx.Done():
			log.Debugf("Stopping reservation manager")
			return nil
		}
	}
}

// newReservationFromNtfn creates a new reservation from the reservation
// notification.
func (m *Manager) newReservationFromNtfn(ctx context.Context,
	currentHeight uint32, req *reservationrpc.ServerReservationNotification,
) (*FSM, error) {

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
	reservationFSM := NewFSM(m.cfg, ProtocolVersionServerInitiated)

	// Add the reservation to the active reservations map.
	m.Lock()
	m.activeReservations[reservationID] = reservationFSM
	m.Unlock()

	initContext := &ServerRequestedInitContext{
		reservationID: reservationID,
		serverPubkey:  serverKey,
		value:         btcutil.Amount(req.Value),
		expiry:        req.Expiry,
		heightHint:    currentHeight,
	}

	// Send the init event to the state machine.
	go func() {
		err = reservationFSM.SendEvent(ctx, OnServerRequest, initContext)
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

// RequestReservationFromServer sends a request to the server to create a new
// reservation.
func (m *Manager) RequestReservationFromServer(ctx context.Context,
	value btcutil.Amount, expiry uint32, maxPrepaymentAmt btcutil.Amount) (
	*Reservation, error) {

	m.Lock()
	currentHeight := m.currentHeight
	m.Unlock()
	// Create a new reservation req.
	req := &ClientRequestedInitContext{
		value:            value,
		relativeExpiry:   expiry,
		heightHint:       uint32(currentHeight),
		maxPrepaymentAmt: maxPrepaymentAmt,
	}

	reservationFSM := NewFSM(m.cfg, ProtocolVersionClientInitiated)
	// Send the event to the main loop.
	m.reqChan <- &FSMSendEventReq{
		fsm:      reservationFSM,
		event:    OnClientInitialized,
		eventCtx: req,
	}

	// We'll now wait for the reservation to be in the state where we are
	// sending the prepayment.
	err := reservationFSM.DefaultObserver.WaitForState(
		ctx, defaultWaitForStateTime, SendPrepaymentPayment,
		fsm.WithAbortEarlyOnErrorOption(),
	)
	if err != nil {
		return nil, err
	}

	// Now we can add the reservation to our active fsm.
	m.Lock()
	m.activeReservations[reservationFSM.reservation.ID] = reservationFSM
	m.Unlock()

	return reservationFSM.reservation, nil
}

// QuoteReservation quotes the server for a new reservation.
func (m *Manager) QuoteReservation(ctx context.Context, value btcutil.Amount,
	expiry uint32) (btcutil.Amount, error) {

	quoteReq := &swapserverrpc.QuoteReservationRequest{
		Value:  uint64(value),
		Expiry: expiry,
	}

	req, err := m.cfg.ReservationClient.QuoteReservation(ctx, quoteReq)
	if err != nil {
		return 0, err
	}

	return btcutil.Amount(req.PrepayCost), nil
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

		m.activeReservations[reservation.ID] = reservationFSM

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
		return fmt.Errorf("reservation not found")
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
		return fmt.Errorf("reservation not found")
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
