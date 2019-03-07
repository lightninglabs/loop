package loop

import (
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
)

// storeMock implements a mock client swap store.
type storeMock struct {
	loopOutSwaps      map[lntypes.Hash]*loopdb.LoopOutContract
	loopOutUpdates    map[lntypes.Hash][]loopdb.SwapState
	loopOutStoreChan  chan loopdb.LoopOutContract
	loopOutUpdateChan chan loopdb.SwapState

	t *testing.T
}

type finishData struct {
	preimage lntypes.Hash
	result   loopdb.SwapState
}

// NewStoreMock instantiates a new mock store.
func newStoreMock(t *testing.T) *storeMock {
	return &storeMock{
		loopOutStoreChan:  make(chan loopdb.LoopOutContract, 1),
		loopOutUpdateChan: make(chan loopdb.SwapState, 1),
		loopOutSwaps:      make(map[lntypes.Hash]*loopdb.LoopOutContract),
		loopOutUpdates:    make(map[lntypes.Hash][]loopdb.SwapState),

		t: t,
	}
}

// FetchLoopOutSwaps returns all swaps currently in the store.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *storeMock) FetchLoopOutSwaps() ([]*loopdb.LoopOut, error) {
	result := []*loopdb.LoopOut{}

	for hash, contract := range s.loopOutSwaps {
		updates := s.loopOutUpdates[hash]
		events := make([]*loopdb.LoopOutEvent, len(updates))
		for i, u := range updates {
			events[i] = &loopdb.LoopOutEvent{
				State: u,
			}
		}

		swap := &loopdb.LoopOut{
			Hash:     hash,
			Contract: contract,
			Events:   events,
		}
		result = append(result, swap)
	}

	return result, nil
}

// CreateLoopOut adds an initiated swap to the store.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *storeMock) CreateLoopOut(hash lntypes.Hash,
	swap *loopdb.LoopOutContract) error {

	_, ok := s.loopOutSwaps[hash]
	if ok {
		return errors.New("swap already exists")
	}

	s.loopOutSwaps[hash] = swap
	s.loopOutUpdates[hash] = []loopdb.SwapState{}
	s.loopOutStoreChan <- *swap

	return nil
}

// UpdateLoopOut stores a new event for a target loop out swap. This appends to
// the event log for a particular swap as it goes through the various stages in
// its lifetime.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *storeMock) UpdateLoopOut(hash lntypes.Hash, time time.Time,
	state loopdb.SwapState) error {

	updates, ok := s.loopOutUpdates[hash]
	if !ok {
		return errors.New("swap does not exists")
	}

	updates = append(updates, state)
	s.loopOutUpdates[hash] = updates
	s.loopOutUpdateChan <- state

	return nil
}

func (s *storeMock) Close() error {
	return nil
}

func (s *storeMock) isDone() error {
	select {
	case <-s.loopOutStoreChan:
		return errors.New("storeChan not empty")
	default:
	}

	select {
	case <-s.loopOutUpdateChan:
		return errors.New("updateChan not empty")
	default:
	}
	return nil
}

func (s *storeMock) assertLoopOutStored() {
	s.t.Helper()

	select {
	case <-s.loopOutStoreChan:
	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be stored")
	}
}

func (s *storeMock) assertStorePreimageReveal() {

	s.t.Helper()

	select {
	case state := <-s.loopOutUpdateChan:
		if state != loopdb.StatePreimageRevealed {
			s.t.Fatalf("unexpected state")
		}
	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be marked as preimage revealed")
	}
}

func (s *storeMock) assertStoreFinished(expectedResult loopdb.SwapState) {
	s.t.Helper()

	select {
	case state := <-s.loopOutUpdateChan:
		if state != expectedResult {
			s.t.Fatalf("expected result %v, but got %v",
				expectedResult, state)
		}
	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be finished")
	}
}
