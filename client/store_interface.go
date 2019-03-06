package client

import (
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
)

// swapClientStore provides persistent storage for swaps.
type swapClientStore interface {
	// getUnchargeSwaps returns all swaps currently in the store.
	getUnchargeSwaps() ([]*PersistentUncharge, error)

	// createUncharge adds an initiated swap to the store.
	createUncharge(hash lntypes.Hash, swap *UnchargeContract) error

	// updateUncharge stores a swap updateUncharge.
	updateUncharge(hash lntypes.Hash, time time.Time, state SwapState) error
}

// PersistentUnchargeEvent contains the dynamic data of a swap.
type PersistentUnchargeEvent struct {
	State SwapState
	Time  time.Time
}

// PersistentUncharge is a combination of the contract and the updates.
type PersistentUncharge struct {
	Hash lntypes.Hash

	Contract *UnchargeContract
	Events   []*PersistentUnchargeEvent
}

// State returns the most recent state of this swap.
func (s *PersistentUncharge) State() SwapState {
	lastUpdate := s.LastUpdate()
	if lastUpdate == nil {
		return StateInitiated
	}

	return lastUpdate.State
}

// LastUpdate returns the most recent update of this swap.
func (s *PersistentUncharge) LastUpdate() *PersistentUnchargeEvent {
	eventCount := len(s.Events)

	if eventCount == 0 {
		return nil
	}

	lastEvent := s.Events[eventCount-1]
	return lastEvent
}

// LastUpdateTime returns the last update time of this swap.
func (s *PersistentUncharge) LastUpdateTime() time.Time {
	lastUpdate := s.LastUpdate()
	if lastUpdate == nil {
		return s.Contract.InitiationTime
	}

	return lastUpdate.Time
}
