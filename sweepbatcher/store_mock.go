package sweepbatcher

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
)

// StoreMock implements a mock client swap store.
type StoreMock struct {
	batches map[int32]dbBatch
	sweeps  map[lntypes.Hash]dbSweep
	mu      sync.Mutex
}

// NewStoreMock instantiates a new mock store.
func NewStoreMock() *StoreMock {
	return &StoreMock{
		batches: make(map[int32]dbBatch),
		sweeps:  make(map[lntypes.Hash]dbSweep),
	}
}

// FetchUnconfirmedSweepBatches fetches all the loop out sweep batches from the
// database that are not in a confirmed state.
func (s *StoreMock) FetchUnconfirmedSweepBatches(ctx context.Context) (
	[]*dbBatch, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	result := []*dbBatch{}
	for _, batch := range s.batches {
		batch := batch
		if batch.State != "confirmed" {
			result = append(result, &batch)
		}
	}

	return result, nil
}

// InsertSweepBatch inserts a batch into the database, returning the id of the
// inserted batch.
func (s *StoreMock) InsertSweepBatch(ctx context.Context,
	batch *dbBatch) (int32, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	var id int32

	if len(s.batches) == 0 {
		id = 0
	} else {
		id = int32(len(s.batches))
	}

	s.batches[id] = *batch
	return id, nil
}

// DropBatch drops a batch from the database.
func (s *StoreMock) DropBatch(ctx context.Context, id int32) error {
	delete(s.batches, id)
	return nil
}

// UpdateSweepBatch updates a batch in the database.
func (s *StoreMock) UpdateSweepBatch(ctx context.Context,
	batch *dbBatch) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.batches[batch.ID] = *batch
	return nil
}

// ConfirmBatch confirms a batch.
func (s *StoreMock) ConfirmBatch(ctx context.Context, id int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	batch, ok := s.batches[id]
	if !ok {
		return errors.New("batch not found")
	}

	batch.State = "confirmed"
	s.batches[batch.ID] = batch

	return nil
}

// FetchBatchSweeps fetches all the sweeps that belong to a batch.
func (s *StoreMock) FetchBatchSweeps(ctx context.Context,
	id int32) ([]*dbSweep, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	result := []*dbSweep{}
	for _, sweep := range s.sweeps {
		sweep := sweep
		if sweep.BatchID == id {
			result = append(result, &sweep)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})

	return result, nil
}

// UpsertSweep inserts a sweep into the database, or updates an existing sweep.
func (s *StoreMock) UpsertSweep(ctx context.Context, sweep *dbSweep) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweeps[sweep.SwapHash] = *sweep

	return nil
}

// GetSweepStatus returns the status of a sweep.
func (s *StoreMock) GetSweepStatus(ctx context.Context,
	swapHash lntypes.Hash) (bool, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	sweep, ok := s.sweeps[swapHash]
	if !ok {
		return false, nil
	}

	return sweep.Completed, nil
}

// Close closes the store.
func (s *StoreMock) Close() error {
	return nil
}

// AssertSweepStored asserts that a sweep is stored.
func (s *StoreMock) AssertSweepStored(id lntypes.Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.sweeps[id]
	return ok
}

// GetParentBatch returns the parent batch of a swap.
func (s *StoreMock) GetParentBatch(ctx context.Context, swapHash lntypes.Hash) (
	*dbBatch, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sweep := range s.sweeps {
		if sweep.SwapHash == swapHash {
			batch, ok := s.batches[sweep.BatchID]
			if !ok {
				return nil, errors.New("batch not found")
			}
			return &batch, nil
		}
	}

	return nil, errors.New("batch not found")
}

// TotalSweptAmount returns the total amount of BTC that has been swept from a
// batch.
func (s *StoreMock) TotalSweptAmount(ctx context.Context, batchID int32) (
	btcutil.Amount, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	batch, ok := s.batches[batchID]
	if !ok {
		return 0, errors.New("batch not found")
	}

	if batch.State != batchConfirmed && batch.State != batchClosed {
		return 0, nil
	}

	var total btcutil.Amount
	for _, sweep := range s.sweeps {
		if sweep.BatchID == batchID {
			total += sweep.Amount
		}
	}

	return 0, nil
}
