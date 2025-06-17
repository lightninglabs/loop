package sweepbatcher

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
)

// StoreMock implements a mock client swap store.
type StoreMock struct {
	batches map[int32]dbBatch
	sweeps  map[wire.OutPoint]dbSweep
	mu      sync.Mutex
	sweepID int32
	batchID int32
}

// NewStoreMock instantiates a new mock store.
func NewStoreMock() *StoreMock {
	return &StoreMock{
		batches: make(map[int32]dbBatch),
		sweeps:  make(map[wire.OutPoint]dbSweep),
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
		if !batch.Confirmed {
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

	id := s.batchID
	s.batchID++

	s.batches[id] = *batch
	return id, nil
}

// CancelBatch drops a batch from the database.
func (s *StoreMock) CancelBatch(ctx context.Context, id int32) error {
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

	batch.Confirmed = true
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

	sweepCopy := *sweep

	if old, exists := s.sweeps[sweep.Outpoint]; exists {
		// Preserve existing sweep ID.
		sweepCopy.ID = old.ID
	} else {
		// Assign fresh sweep ID.
		sweepCopy.ID = s.sweepID
		s.sweepID++
	}

	s.sweeps[sweep.Outpoint] = sweepCopy

	return nil
}

// GetSweepStatus returns the status of a sweep.
func (s *StoreMock) GetSweepStatus(ctx context.Context,
	outpoint wire.OutPoint) (bool, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	sweep, ok := s.sweeps[outpoint]
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
func (s *StoreMock) AssertSweepStored(outpoint wire.OutPoint) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.sweeps[outpoint]
	return ok
}

// GetParentBatch returns the parent batch of a swap.
func (s *StoreMock) GetParentBatch(ctx context.Context,
	outpoint wire.OutPoint) (*dbBatch, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sweep := range s.sweeps {
		if sweep.Outpoint == outpoint {
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

	if !batch.Confirmed {
		return 0, nil
	}

	var total btcutil.Amount
	for _, sweep := range s.sweeps {
		if sweep.BatchID == batchID {
			total += sweep.Amount
		}
	}

	return total, nil
}
