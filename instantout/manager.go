package instantout

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	defaultStateWaitTime = 30 * time.Second
	defaultCltv          = 100
	ErrSwapDoesNotExist  = errors.New("swap does not exist")
)

// Manager manages the instantout state machines.
type Manager struct {
	sync.Mutex

	// cfg contains all the services that the reservation manager needs to
	// operate.
	cfg *Config

	// activeInstantOuts contains all the active instantouts.
	activeInstantOuts map[lntypes.Hash]*FSM

	// currentHeight stores the currently best known block height.
	currentHeight int32

	// blockEpochChan receives new block heights.
	blockEpochChan chan int32

	runCtx context.Context
}

// NewInstantOutManager creates a new instantout manager.
func NewInstantOutManager(cfg *Config, height int32) *Manager {
	return &Manager{
		cfg:               cfg,
		activeInstantOuts: make(map[lntypes.Hash]*FSM),
		blockEpochChan:    make(chan int32),
		currentHeight:     height,
	}
}

// Run runs the instantout manager.
func (m *Manager) Run(ctx context.Context, initChan chan struct{}) error {
	log.Debugf("Starting instantout manager")
	defer func() {
		log.Debugf("Stopping instantout manager")
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	m.runCtx = runCtx

	err := m.recoverInstantOuts(runCtx)
	if err != nil {
		close(initChan)
		return err
	}

	newBlockChan, newBlockErrChan, err :=
		utils.RegisterBlockEpochNtfnWithRetry(ctx, m.cfg.ChainNotifier)
	if err != nil {
		close(initChan)
		return err
	}

	close(initChan)

	for {
		select {
		case <-runCtx.Done():
			return nil

		case height := <-newBlockChan:
			m.Lock()
			m.currentHeight = height
			m.Unlock()

		case err := <-newBlockErrChan:
			return err
		}
	}
}

// recoverInstantOuts recovers all the active instantouts from the database.
func (m *Manager) recoverInstantOuts(ctx context.Context) error {
	// Fetch all the active instantouts from the database.
	activeInstantOuts, err := m.cfg.Store.ListInstantLoopOuts(ctx)
	if err != nil {
		return err
	}

	for _, instantOut := range activeInstantOuts {
		if isFinalState(instantOut.State) {
			continue
		}

		log.Debugf("Recovering instantout %v", instantOut.SwapHash)

		instantOutFSM, err := NewFSMFromInstantOut(
			m.cfg, instantOut,
		)
		if err != nil {
			return err
		}

		m.activeInstantOuts[instantOut.SwapHash] = instantOutFSM

		// As SendEvent can block, we'll start a goroutine to process
		// the event.
		go func() {
			err := instantOutFSM.SendEvent(ctx, OnRecover, nil)
			if err != nil {
				log.Errorf("FSM %v Error sending recover "+
					"event %v, state: %v",
					instantOutFSM.InstantOut.SwapHash, err,
					instantOutFSM.InstantOut.State)
			}
		}()
	}

	return nil
}

// NewInstantOut creates a new instantout.
func (m *Manager) NewInstantOut(ctx context.Context,
	reservations []reservation.ID, sweepAddress string) (*FSM, error) {

	var (
		sweepAddr btcutil.Address
		err       error
	)
	if sweepAddress != "" {
		sweepAddr, err = btcutil.DecodeAddress(
			sweepAddress, m.cfg.Network,
		)
		if err != nil {
			return nil, err
		}
	}

	m.Lock()
	// Create the instantout request.
	request := &InitInstantOutCtx{
		cltvExpiry:      m.currentHeight + int32(defaultCltv),
		reservations:    reservations,
		initationHeight: m.currentHeight,
		protocolVersion: CurrentProtocolVersion(),
		sweepAddress:    sweepAddr,
	}

	instantOut, err := NewFSM(m.cfg, ProtocolVersionFullReservation)
	if err != nil {
		m.Unlock()
		return nil, err
	}
	m.activeInstantOuts[instantOut.InstantOut.SwapHash] = instantOut
	m.Unlock()

	// Start the instantout FSM.
	go func() {
		err := instantOut.SendEvent(m.runCtx, OnStart, request)
		if err != nil {
			log.Errorf("Error sending event: %v", err)
		}
	}()

	// If everything went well, we'll wait for the instant out to be
	// waiting for sweepless sweep to be confirmed.
	err = instantOut.DefaultObserver.WaitForState(
		ctx, defaultStateWaitTime, WaitForSweeplessSweepConfirmed,
		fsm.WithAbortEarlyOnErrorOption(),
	)
	if err != nil {
		return nil, err
	}

	return instantOut, nil
}

// GetActiveInstantOut returns an active instant out.
func (m *Manager) GetActiveInstantOut(swapHash lntypes.Hash) (*FSM, error) {
	m.Lock()
	defer m.Unlock()

	fsm, ok := m.activeInstantOuts[swapHash]
	if !ok {
		return nil, ErrSwapDoesNotExist
	}

	// If the instant out is in a final state, we'll remove it from the
	// active instant outs.
	if isFinalState(fsm.InstantOut.State) {
		delete(m.activeInstantOuts, swapHash)
	}

	return fsm, nil
}

type Quote struct {
	// ServiceFee is the fee in sat that is paid to the loop service.
	ServiceFee btcutil.Amount

	// OnChainFee is the estimated on chain fee in sat.
	OnChainFee btcutil.Amount
}

// GetInstantOutQuote returns a quote for an instant out.
func (m *Manager) GetInstantOutQuote(ctx context.Context,
	amt btcutil.Amount, reservationIDs [][]byte) (Quote, error) {

	if len(reservationIDs) == 0 {
		return Quote{}, fmt.Errorf("no reservations selected")
	}

	if amt <= 0 {
		return Quote{}, fmt.Errorf("no amount selected")
	}

	// Get the service fee.
	quoteRes, err := m.cfg.InstantOutClient.GetInstantOutQuote(
		ctx, &swapserverrpc.GetInstantOutQuoteRequest{
			Amount:         uint64(amt),
			ReservationIds: reservationIDs,
		},
	)
	if err != nil {
		return Quote{}, err
	}

	// Get the offchain fee by getting the fee estimate from the lnd client
	// and multiplying it by the estimated sweepless sweep transaction.
	feeRate, err := m.cfg.Wallet.EstimateFeeRate(ctx, normalConfTarget)
	if err != nil {
		return Quote{}, err
	}

	// The on chain chainFee is the chainFee rate times the estimated
	// sweepless sweep transaction size.
	chainFee := feeRate.FeeForWeight(sweeplessSweepWeight(len(reservationIDs)))

	return Quote{
		ServiceFee: btcutil.Amount(quoteRes.SwapFee),
		OnChainFee: chainFee,
	}, nil
}

// ListInstantOuts returns all instant outs from the database.
func (m *Manager) ListInstantOuts(ctx context.Context) ([]*InstantOut, error) {
	return m.cfg.Store.ListInstantLoopOuts(ctx)
}
