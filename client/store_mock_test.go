package client

import (
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
)

// storeMock implements a mock client swap store.
type storeMock struct {
	unchargeSwaps      map[lntypes.Hash]*UnchargeContract
	unchargeUpdates    map[lntypes.Hash][]SwapState
	unchargeStoreChan  chan UnchargeContract
	unchargeUpdateChan chan SwapState

	t *testing.T
}

type finishData struct {
	preimage lntypes.Hash
	result   SwapState
}

// NewStoreMock instantiates a new mock store.
func newStoreMock(t *testing.T) *storeMock {
	return &storeMock{
		unchargeStoreChan:  make(chan UnchargeContract, 1),
		unchargeUpdateChan: make(chan SwapState, 1),
		unchargeSwaps:      make(map[lntypes.Hash]*UnchargeContract),
		unchargeUpdates:    make(map[lntypes.Hash][]SwapState),

		t: t,
	}
}

// getUnchargeSwaps returns all swaps currently in the store.
func (s *storeMock) getUnchargeSwaps() ([]*PersistentUncharge, error) {
	result := []*PersistentUncharge{}

	for hash, contract := range s.unchargeSwaps {
		updates := s.unchargeUpdates[hash]
		events := make([]*PersistentUnchargeEvent, len(updates))
		for i, u := range updates {
			events[i] = &PersistentUnchargeEvent{
				State: u,
			}
		}

		swap := &PersistentUncharge{
			Hash:     hash,
			Contract: contract,
			Events:   events,
		}
		result = append(result, swap)
	}

	return result, nil
}

// createUncharge adds an initiated swap to the store.
func (s *storeMock) createUncharge(hash lntypes.Hash,
	swap *UnchargeContract) error {

	_, ok := s.unchargeSwaps[hash]
	if ok {
		return errors.New("swap already exists")
	}

	s.unchargeSwaps[hash] = swap
	s.unchargeUpdates[hash] = []SwapState{}
	s.unchargeStoreChan <- *swap

	return nil
}

// Finalize stores the final swap result.
func (s *storeMock) updateUncharge(hash lntypes.Hash, time time.Time,
	state SwapState) error {

	updates, ok := s.unchargeUpdates[hash]
	if !ok {
		return errors.New("swap does not exists")
	}

	updates = append(updates, state)
	s.unchargeUpdates[hash] = updates
	s.unchargeUpdateChan <- state

	return nil
}

func (s *storeMock) isDone() error {
	select {
	case <-s.unchargeStoreChan:
		return errors.New("storeChan not empty")
	default:
	}

	select {
	case <-s.unchargeUpdateChan:
		return errors.New("updateChan not empty")
	default:
	}
	return nil
}

func (s *storeMock) assertUnchargeStored() {
	s.t.Helper()

	select {
	case <-s.unchargeStoreChan:
	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be stored")
	}
}

func (s *storeMock) assertStorePreimageReveal() {

	s.t.Helper()

	select {
	case state := <-s.unchargeUpdateChan:
		if state != StatePreimageRevealed {
			s.t.Fatalf("unexpected state")
		}
	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be marked as preimage revealed")
	}
}

func (s *storeMock) assertStoreFinished(expectedResult SwapState) {
	s.t.Helper()

	select {
	case state := <-s.unchargeUpdateChan:
		if state != expectedResult {
			s.t.Fatalf("expected result %v, but got %v",
				expectedResult, state)
		}
	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be finished")
	}
}
