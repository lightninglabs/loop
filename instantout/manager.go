package instantout

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	defaultStateWaitTime = 30 * time.Second
	defaultCltv          = 100
	ErrSwapDoesNotExist  = errors.New("swap does not exist")
)

// Manager manages the instantout state machines.
type Manager struct {
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

	sync.Mutex
}

// NewInstantOutManager creates a new instantout manager.
func NewInstantOutManager(cfg *Config) *Manager {
	return &Manager{
		cfg:               cfg,
		activeInstantOuts: make(map[lntypes.Hash]*FSM),
		blockEpochChan:    make(chan int32),
	}
}

// Run runs the reservation manager.
func (i *Manager) Run(ctx context.Context, height int32) error {
	log.Debugf("Starting reservation manager")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	i.Lock()
	i.runCtx = runCtx
	i.currentHeight = height
	i.Unlock()

	err := i.RecoverInstantOuts(runCtx)
	if err != nil {
		return err
	}

	newBlockChan, newBlockErrChan, err := i.cfg.ChainNotifier.
		RegisterBlockEpochNtfn(ctx)
	if err != nil {
		return err
	}

	for {
		select {
		case <-runCtx.Done():
			log.Debugf("Stopping reservation manager")
			return nil

		case height := <-newBlockChan:
			i.Lock()
			i.currentHeight = height
			i.Unlock()

		case err := <-newBlockErrChan:
			return err
		}
	}
}

// RecoverInstantOuts recovers all the active instantouts from the database.
func (i *Manager) RecoverInstantOuts(ctx context.Context) error {
	// Fetch all the active instantouts from the database.
	activeInstantOuts, err := i.cfg.Store.ListInstantLoopOuts(ctx)
	if err != nil {
		return err
	}

	for _, instantOut := range activeInstantOuts {
		if isFinalState(instantOut.State) {
			continue
		}

		log.Debugf("Recovering instantout %v", instantOut.SwapHash)

		instantOutFSM, err := NewFSMFromInstantOut(
			ctx, i.cfg, instantOut,
		)
		if err != nil {
			return err
		}

		i.activeInstantOuts[instantOut.SwapHash] = instantOutFSM

		// As SendEvent can block, we'll start a goroutine to process
		// the event.
		go func() {
			err := instantOutFSM.SendEvent(OnRecover, nil)
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
func (i *Manager) NewInstantOut(ctx context.Context,
	reservations []reservation.ID) (*FSM, error) {

	i.Lock()
	// Create the instantout request.
	request := &InitInstantOutCtx{
		cltvExpiry:      i.currentHeight + int32(defaultCltv),
		reservations:    reservations,
		initationHeight: i.currentHeight,
	}

	instantOut, err := NewFSM(
		i.runCtx, i.cfg, ProtocolVersionFullReservation,
	)
	if err != nil {
		i.Unlock()
		return nil, err
	}
	i.Unlock()

	// Start the instantout FSM.
	go func() {
		err := instantOut.SendEvent(OnStart, request)
		if err != nil {
			log.Errorf("Error sending event: %v", err)
		}
	}()

	// If everything went well, we'll wait for the instant out to be
	// waiting for sweepless sweep to be confirmed.
	err = instantOut.DefaultObserver.WaitForState(
		ctx, defaultStateWaitTime, WaitForSweeplessSweepConfirmed,
	)
	if err != nil {
		return instantOut, err
	}

	return instantOut, nil
}

// GetActiveInstantOut returns an active instant out.
func (i *Manager) GetActiveInstantOut(swapHash lntypes.Hash) (*FSM, error) {
	i.Lock()
	defer i.Unlock()

	reservation, ok := i.activeInstantOuts[swapHash]
	if !ok {
		return nil, ErrSwapDoesNotExist
	}

	return reservation, nil
}
