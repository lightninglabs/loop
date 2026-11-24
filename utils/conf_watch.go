package utils

import (
	"context"
	"errors"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
)

// DefaultConfWatchRetryDelay is the retry delay of a confirmation watch that
// doesn't set one.
const DefaultConfWatchRetryDelay = 10 * time.Second

var (
	// errConfChanClosed is reported when the notifier closes the
	// confirmation channel without a confirmation.
	errConfChanClosed = errors.New("confirmation channel closed")

	// errNilConf is reported when the notifier sends a nil confirmation.
	errNilConf = errors.New("nil confirmation")

	// errConfErrChanClosed is reported when the notifier closes the error
	// channel without an error.
	errConfErrChanClosed = errors.New("confirmation error channel closed")
)

// ConfRegistrar is the part of the chain notifier a confirmation watch needs.
type ConfRegistrar interface {
	// RegisterConfirmationsNtfn registers for the confirmation of a
	// transaction.
	RegisterConfirmationsNtfn(ctx context.Context, txid *chainhash.Hash,
		pkScript []byte, numConfs, heightHint int32,
		opts ...lndclient.NotifierOption) (
		chan *chainntnfs.TxConfirmation, chan error, error)
}

// ConfWatch describes the confirmation of one transaction to watch.
type ConfWatch struct {
	// Notifier is the chain notifier to register with.
	Notifier ConfRegistrar

	// Clock times the retry delay. The default clock is used if it is
	// nil.
	Clock clock.Clock

	// Txid is the transaction to watch. It must be set: without it the
	// notifier would report any transaction that pays to PkScript.
	Txid *chainhash.Hash

	// PkScript is a script of one of the transaction's outputs.
	PkScript []byte

	// NumConfs is the number of confirmations to wait for.
	NumConfs int32

	// HeightHint is the earliest height the transaction can be in.
	HeightHint int32

	// Opts are passed to every registration. The registration is released
	// once the confirmation arrives, so options that report later events,
	// like lndclient.WithReOrgChan, are not supported.
	Opts []lndclient.NotifierOption

	// RetryDelay is the time to wait after a failed attempt before
	// registering again. DefaultConfWatchRetryDelay is used if it isn't
	// positive.
	RetryDelay time.Duration

	// OnRetry, if set, is called after every failed attempt, before the
	// retry delay. The attempt counts the failures of this watch and
	// starts at one. It is called from the goroutine of the watch.
	OnRetry func(attempt int, err error)
}

// WatchConfirmation watches a transaction until it confirms. It registers
// again after a delay when the registration fails or the notifier reports an
// error, so a notifier outage doesn't end the watch. The confirmation is sent
// once on the returned channel, which is never closed. The watch runs in its
// own goroutine and stops when the context is canceled, so the caller must
// cancel the context when it no longer needs the watch.
func WatchConfirmation(ctx context.Context,
	watch ConfWatch) <-chan *chainntnfs.TxConfirmation {

	if watch.Clock == nil {
		watch.Clock = clock.NewDefaultClock()
	}
	if watch.RetryDelay <= 0 {
		watch.RetryDelay = DefaultConfWatchRetryDelay
	}

	confChan := make(chan *chainntnfs.TxConfirmation, 1)

	go func() {
		for attempt := 1; ; attempt++ {
			conf, err := watch.waitOnce(ctx)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				confChan <- conf

				return
			}

			if watch.OnRetry != nil {
				watch.OnRetry(attempt, err)
			}

			select {
			case <-ctx.Done():
				return

			case <-watch.Clock.TickAfter(watch.RetryDelay):
			}

			// The delay and the cancel can be ready together.
			if ctx.Err() != nil {
				return
			}
		}
	}()

	return confChan
}

// waitOnce registers for the confirmation once and waits for it or for an
// error. The registration is released when it returns.
func (w *ConfWatch) waitOnce(ctx context.Context) (*chainntnfs.TxConfirmation,
	error) {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	confChan, errChan, err := w.Notifier.RegisterConfirmationsNtfn(
		ctx, w.Txid, w.PkScript, w.NumConfs, w.HeightHint, w.Opts...,
	)
	if err != nil {
		return nil, err
	}

	select {
	case conf, ok := <-confChan:
		if !ok {
			return nil, errConfChanClosed
		}
		if conf == nil {
			return nil, errNilConf
		}

		return conf, nil

	case err, ok := <-errChan:
		if !ok || err == nil {
			return nil, errConfErrChanClosed
		}

		return nil, err

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
