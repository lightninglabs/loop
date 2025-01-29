package loopdb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// StoreMock implements a mock client swap store.
type StoreMock struct {
	sync.RWMutex

	LoopOutSwaps      map[lntypes.Hash]*LoopOutContract
	LoopOutUpdates    map[lntypes.Hash][]SwapStateData
	loopOutStoreChan  chan LoopOutContract
	loopOutUpdateChan chan SwapStateData

	LoopInSwaps      map[lntypes.Hash]*LoopInContract
	LoopInUpdates    map[lntypes.Hash][]SwapStateData
	loopInStoreChan  chan LoopInContract
	loopInUpdateChan chan SwapStateData

	migrations map[string]struct{}

	t *testing.T
}

// NewStoreMock instantiates a new mock store.
func NewStoreMock(t *testing.T) *StoreMock {
	return &StoreMock{
		loopOutStoreChan:  make(chan LoopOutContract, 1),
		loopOutUpdateChan: make(chan SwapStateData, 1),
		LoopOutSwaps:      make(map[lntypes.Hash]*LoopOutContract),
		LoopOutUpdates:    make(map[lntypes.Hash][]SwapStateData),

		loopInStoreChan:  make(chan LoopInContract, 1),
		loopInUpdateChan: make(chan SwapStateData, 1),
		LoopInSwaps:      make(map[lntypes.Hash]*LoopInContract),
		LoopInUpdates:    make(map[lntypes.Hash][]SwapStateData),
		migrations:       make(map[string]struct{}),
		t:                t,
	}
}

// FetchLoopOutSwaps returns all swaps currently in the store.
//
// NOTE: Part of the SwapStore interface.
func (s *StoreMock) FetchLoopOutSwaps(ctx context.Context) ([]*LoopOut, error) {
	s.RLock()
	defer s.RUnlock()

	result := []*LoopOut{}

	for hash, contract := range s.LoopOutSwaps {
		updates := s.LoopOutUpdates[hash]
		events := make([]*LoopEvent, len(updates))
		for i, u := range updates {
			events[i] = &LoopEvent{
				SwapStateData: u,
			}
		}

		swap := &LoopOut{
			Loop: Loop{
				Hash:   hash,
				Events: events,
			},
			Contract: contract,
		}
		result = append(result, swap)
	}

	return result, nil
}

// FetchLoopOutSwaps returns all swaps currently in the store.
//
// NOTE: Part of the SwapStore interface.
func (s *StoreMock) FetchLoopOutSwap(ctx context.Context,
	hash lntypes.Hash) (*LoopOut, error) {

	s.RLock()
	defer s.RUnlock()

	contract, ok := s.LoopOutSwaps[hash]
	if !ok {
		return nil, errors.New("swap not found")
	}

	updates := s.LoopOutUpdates[hash]
	events := make([]*LoopEvent, len(updates))
	for i, u := range updates {
		events[i] = &LoopEvent{
			SwapStateData: u,
		}
	}

	swap := &LoopOut{
		Loop: Loop{
			Hash:   hash,
			Events: events,
		},
		Contract: contract,
	}

	return swap, nil
}

// CreateLoopOut adds an initiated swap to the store.
//
// NOTE: Part of the SwapStore interface.
func (s *StoreMock) CreateLoopOut(ctx context.Context, hash lntypes.Hash,
	swap *LoopOutContract) error {

	s.Lock()
	defer s.Unlock()

	_, ok := s.LoopOutSwaps[hash]
	if ok {
		return errors.New("swap already exists")
	}

	s.LoopOutSwaps[hash] = swap
	s.LoopOutUpdates[hash] = []SwapStateData{}
	s.loopOutStoreChan <- *swap

	return nil
}

// FetchLoopInSwaps returns all in swaps currently in the store.
func (s *StoreMock) FetchLoopInSwaps(ctx context.Context) ([]*LoopIn,
	error) {

	s.RLock()
	defer s.RUnlock()

	result := []*LoopIn{}

	for hash, contract := range s.LoopInSwaps {
		updates := s.LoopInUpdates[hash]
		events := make([]*LoopEvent, len(updates))
		for i, u := range updates {
			events[i] = &LoopEvent{
				SwapStateData: u,
			}
		}

		swap := &LoopIn{
			Loop: Loop{
				Hash:   hash,
				Events: events,
			},
			Contract: contract,
		}
		result = append(result, swap)
	}

	return result, nil
}

func (s *StoreMock) UpdateLoopOutAssetInfo(ctx context.Context,
	hash lntypes.Hash, asset *LoopOutAssetSwap) error {

	return errors.New("not implemented")
}

// CreateLoopIn adds an initiated loop in swap to the store.
//
// NOTE: Part of the SwapStore interface.
func (s *StoreMock) CreateLoopIn(ctx context.Context, hash lntypes.Hash,
	swap *LoopInContract) error {

	s.Lock()
	defer s.Unlock()

	_, ok := s.LoopInSwaps[hash]
	if ok {
		return errors.New("swap already exists")
	}

	s.LoopInSwaps[hash] = swap
	s.LoopInUpdates[hash] = []SwapStateData{}
	s.loopInStoreChan <- *swap

	return nil
}

// UpdateLoopOut stores a new event for a target loop out swap. This appends to
// the event log for a particular swap as it goes through the various stages in
// its lifetime.
//
// NOTE: Part of the SwapStore interface.
func (s *StoreMock) UpdateLoopOut(ctx context.Context, hash lntypes.Hash,
	time time.Time, state SwapStateData) error {

	s.Lock()
	defer s.Unlock()

	updates, ok := s.LoopOutUpdates[hash]
	if !ok {
		return errors.New("swap does not exists")
	}

	updates = append(updates, state)
	s.LoopOutUpdates[hash] = updates
	s.loopOutUpdateChan <- state

	return nil
}

// UpdateLoopIn stores a new event for a target loop in swap. This appends to
// the event log for a particular swap as it goes through the various stages in
// its lifetime.
//
// NOTE: Part of the SwapStore interface.
func (s *StoreMock) UpdateLoopIn(ctx context.Context, hash lntypes.Hash,
	time time.Time, state SwapStateData) error {

	s.Lock()
	defer s.Unlock()

	updates, ok := s.LoopInUpdates[hash]
	if !ok {
		return errors.New("swap does not exists")
	}

	updates = append(updates, state)
	s.LoopInUpdates[hash] = updates
	s.loopInUpdateChan <- state

	return nil
}

// PutLiquidityParams writes the serialized `manager.Parameters` bytes into the
// bucket.
//
// NOTE: Part of the SwapStore interface.
func (s *StoreMock) PutLiquidityParams(ctx context.Context, assetId string,
	params []byte) error {

	return nil
}

// FetchLiquidityParams reads the serialized `manager.Parameters` bytes from
// the bucket.
//
// NOTE: Part of the SwapStore interface.
func (s *StoreMock) FetchLiquidityParams(ctx context.Context) (
	[]sqlc.LiquidityParam, error) {

	return nil, nil
}

// Close closes the store.
func (s *StoreMock) Close() error {
	return nil
}

// isDone asserts that the store mock has no pending operations.
func (s *StoreMock) IsDone() error {
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

// AssertLoopOutStored asserts that a swap is stored.
func (s *StoreMock) AssertLoopOutStored() {
	s.t.Helper()

	select {
	case <-s.loopOutStoreChan:
	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be stored")
	}
}

// AssertLoopOutState asserts that a specified state transition is persisted to
// disk.
func (s *StoreMock) AssertLoopOutState(expectedState SwapState) {
	s.t.Helper()

	select {
	case state := <-s.loopOutUpdateChan:
		require.Equal(s.t, expectedState, state.State)
	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap state to be stored")
	}
}

// AssertLoopInStored asserts that a loop-in swap is stored.
func (s *StoreMock) AssertLoopInStored() {
	s.t.Helper()

	select {
	case <-s.loopInStoreChan:
	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be stored")
	}
}

// assertLoopInState asserts that a specified state transition is persisted to
// disk.
func (s *StoreMock) AssertLoopInState(
	expectedState SwapState) SwapStateData {

	s.t.Helper()

	state := <-s.loopInUpdateChan
	require.Equal(s.t, expectedState, state.State)

	return state
}

// AssertStorePreimageReveal asserts that a swap is marked as preimage revealed.
func (s *StoreMock) AssertStorePreimageReveal() {
	s.t.Helper()

	select {
	case state := <-s.loopOutUpdateChan:
		require.Equal(s.t, StatePreimageRevealed, state.State)

	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be marked as preimage revealed")
	}
}

// AssertStoreFinished asserts that a swap is marked as finished.
func (s *StoreMock) AssertStoreFinished(expectedResult SwapState) {
	s.t.Helper()

	select {
	case state := <-s.loopOutUpdateChan:
		require.Equal(s.t, expectedResult, state.State)

	case <-time.After(test.Timeout):
		s.t.Fatalf("expected swap to be finished")
	}
}

// BatchCreateLoopOut creates many loop out swaps in a batch.
func (b *StoreMock) BatchCreateLoopOut(ctx context.Context,
	swaps map[lntypes.Hash]*LoopOutContract) error {

	return errors.New("not implemented")
}

// BatchCreateLoopIn creates many loop in swaps in a batch.
func (b *StoreMock) BatchCreateLoopIn(ctx context.Context,
	swaps map[lntypes.Hash]*LoopInContract) error {

	return errors.New("not implemented")
}

// BatchInsertUpdate inserts many updates for a swap in a batch.
func (b *StoreMock) BatchInsertUpdate(ctx context.Context,
	updateData map[lntypes.Hash][]BatchInsertUpdateData) error {

	return errors.New("not implemented")
}

// BatchUpdateLoopOutSwapCosts updates the swap costs for a batch of loop out
// swaps.
func (s *StoreMock) BatchUpdateLoopOutSwapCosts(ctx context.Context,
	costs map[lntypes.Hash]SwapCost) error {

	s.Lock()
	defer s.Unlock()

	for hash, cost := range costs {
		if _, ok := s.LoopOutUpdates[hash]; !ok {
			return fmt.Errorf("swap has no updates: %v", hash)
		}

		updates, ok := s.LoopOutUpdates[hash]
		if !ok {
			return fmt.Errorf("swap has no updates: %v", hash)
		}

		updates[len(updates)-1].Cost = cost
	}

	return nil
}

// HasMigration returns true if the migration with the given ID has been done.
func (s *StoreMock) HasMigration(ctx context.Context, migrationID string) (
	bool, error) {

	s.RLock()
	defer s.RUnlock()

	_, ok := s.migrations[migrationID]

	return ok, nil
}

// SetMigration marks the migration with the given ID as done.
func (s *StoreMock) SetMigration(ctx context.Context,
	migrationID string) error {

	s.Lock()
	defer s.Unlock()

	if _, ok := s.migrations[migrationID]; ok {
		return errors.New("migration already done")
	}

	s.migrations[migrationID] = struct{}{}

	return nil
}
