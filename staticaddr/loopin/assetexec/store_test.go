package assetexec

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"sync"
)

// memoryStore isolates controller policy tests from the SQL implementation.
// Actual filesystem restart and atomic inventory tests live in loopin.
type memoryStore struct {
	mu         sync.Mutex
	records    map[[32]byte][]byte
	releaseErr error
	releases   int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{records: make(map[[32]byte][]byte)}
}

func (s *memoryStore) Create(ctx context.Context, r *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateCreate(r); err != nil {
		return err
	}
	if _, exists := s.records[r.Offer.Hash]; exists {
		return ErrConflict
	}
	for _, raw := range s.records {
		old, err := DecodeRecord(raw)
		if err != nil {
			return err
		}
		if old.DepositsReleased {
			continue
		}
		for _, a := range old.Offer.Terms.DepositOutpoints {
			if slices.Contains(r.Offer.Terms.DepositOutpoints, a) {
				return ErrConflict
			}
		}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.records[r.Offer.Hash] = raw
	return nil
}
func (s *memoryStore) Get(ctx context.Context, h [32]byte) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return DecodeRecord(s.records[h])
}

func (s *memoryStore) Update(
	ctx context.Context,
	r *Record,
	previous int64,
) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if previous < 0 || previous == math.MaxInt64 || r.Revision != previous+1 {
		return ErrInvalid
	}
	if err := ValidateRecord(r); err != nil {
		return err
	}
	old, err := DecodeRecord(s.records[r.Offer.Hash])
	if err != nil {
		return err
	}
	if old.Revision != previous {
		return ErrConflict
	}
	if err := ValidateUpdate(old, r); err != nil {
		return err
	}
	if r.DepositsReleased && !old.DepositsReleased {
		if s.releaseErr != nil {
			return s.releaseErr
		}
		s.releases++
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.records[r.Offer.Hash] = raw
	return nil
}
func (s *memoryStore) ListActive(ctx context.Context) ([]*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var active []*Record
	for _, raw := range s.records {
		r, err := DecodeRecord(raw)
		if err != nil {
			return nil, err
		}
		if !r.State.Terminal() || !r.DepositsReleased {
			active = append(active, r)
		}
	}
	return active, nil
}
