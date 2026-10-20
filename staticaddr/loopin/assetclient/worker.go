package assetclient

import (
	"context"
	"sync"
	"time"

	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
)

// WorkerConfig controls recovery of the client's durable active swaps.
// Initialized must close only after the wallet, deposit ownership manager,
// signer, chain driver and authenticated RPC clients are ready. OnError may
// run concurrently and must not log private invoices or recovery material.
type WorkerConfig struct {
	Initialized   <-chan struct{}
	Interval      time.Duration
	ActionTimeout time.Duration
	Concurrency   int
	OnError       func([32]byte, error)

	// OnReady runs once after the initial recovery scan has completed.
	OnReady func()
}

// Run waits for initialization, then recovers active swaps immediately and on
// each interval. The session resumes interrupted preparation, settlement,
// proof confirmation and refunds without selecting new deposits or quotes.
// Each record has its own deadline; individual failures are reported and
// retried. A journal scan failure stops the worker. Run one worker per session
// and wait for it to exit before closing the store or transaction driver.
// All Actions and callbacks must honor cancellation and their deadlines.
func (s *Session) Run(ctx context.Context, cfg WorkerConfig) error {
	if s.Controller == nil || s.Adapter == nil || s.Adapter.Store == nil ||
		cfg.Initialized == nil || cfg.Interval <= 0 ||
		cfg.ActionTimeout <= 0 || cfg.Concurrency <= 0 {

		return assetexec.ErrInvalid
	}
	if !s.running.CompareAndSwap(false, true) {
		return assetexec.ErrState
	}
	defer s.running.Store(false)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-cfg.Initialized:
	}
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		scanCtx, cancel := context.WithTimeout(ctx, cfg.ActionTimeout)
		records, err := s.Adapter.Store.ListActive(scanCtx)
		cancel()
		if err != nil {
			return err
		}
		if err := s.recoverBatch(ctx, cfg, records); err != nil {
			return err
		}
		if first && ctx.Err() == nil && cfg.OnReady != nil {
			cfg.OnReady()
		}
		first = false
		timer := time.NewTimer(cfg.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Session) recoverBatch(ctx context.Context, cfg WorkerConfig,
	records []*assetexec.Record) error {

	for _, record := range records {
		if record == nil {
			return assetexec.ErrInvalid
		}
	}
	jobs := make(chan [32]byte)
	var workers sync.WaitGroup
	for range min(cfg.Concurrency, len(records)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for hash := range jobs {
				if ctx.Err() != nil {
					return
				}
				actionCtx, cancel := context.WithTimeout(
					ctx, cfg.ActionTimeout,
				)
				err := s.Recover(actionCtx, hash)
				cancel()
				if err != nil && cfg.OnError != nil {
					cfg.OnError(hash, err)
				}
			}
		}()
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
