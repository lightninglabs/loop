package assetclient

import (
	"context"
	"sync"
	"time"

	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
)

// RecoverActive resumes one bounded batch from the manager's normal recovery
// loop. Each record gets an independent deadline; a failed action is retried
// by the next batch. Only a journal scan failure stops the owning manager.
func (s *Session) RecoverActive(ctx context.Context, timeout time.Duration,
	concurrency int, onError func([32]byte, error)) error {

	if s.Controller == nil || s.Adapter == nil || s.Adapter.Store == nil ||
		timeout <= 0 || concurrency <= 0 {

		return assetexec.ErrInvalid
	}
	if !s.running.CompareAndSwap(false, true) {
		return assetexec.ErrState
	}
	defer s.running.Store(false)
	if err := ctx.Err(); err != nil {
		return err
	}
	scanCtx, cancel := context.WithTimeout(ctx, timeout)
	records, err := s.Adapter.Store.ListActive(scanCtx)
	cancel()
	if err != nil {
		return err
	}
	return s.recoverBatch(ctx, timeout, concurrency, onError, records)
}

func (s *Session) recoverBatch(ctx context.Context, timeout time.Duration,
	concurrency int, onError func([32]byte, error),
	records []*assetexec.Record) error {

	for _, record := range records {
		if record == nil {
			return assetexec.ErrInvalid
		}
	}
	jobs := make(chan [32]byte)
	var workers sync.WaitGroup
	for range min(concurrency, len(records)) {
		workers.Go(func() {
			for hash := range jobs {
				if ctx.Err() != nil {
					return
				}
				actionCtx, cancel := context.WithTimeout(
					ctx, timeout,
				)
				err := s.Recover(actionCtx, hash)
				cancel()
				if err != nil && onError != nil {
					onError(hash, err)
				}
			}
		})
	}

dispatch:
	for _, record := range records {
		select {
		case jobs <- record.Offer.Hash:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
	return ctx.Err()
}
