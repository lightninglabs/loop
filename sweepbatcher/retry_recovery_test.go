package sweepbatcher

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

type signingFailureHelper struct {
	*mockPresignedHelper

	// signErr is only changed before Run or within the batch event loop.
	signErr error
}

func (h *signingFailureHelper) SignTx(ctx context.Context,
	primarySweepID wire.OutPoint, tx *wire.MsgTx, inputAmt btcutil.Amount,
	minRelayFee, feeRate chainfee.SatPerKWeight,
	loadOnly bool) (*wire.MsgTx, error) {

	if h.signErr != nil {
		return nil, h.signErr
	}

	return h.mockPresignedHelper.SignTx(
		ctx, primarySweepID, tx, inputAmt, minRelayFee, feeRate,
		loadOnly,
	)
}

type batchRetryHarness struct {
	ctx     context.Context
	wallet  *firstMinRelayFailure
	helper  *signingFailureHelper
	batcher *Batcher
	batch   *batch
	logged  []string
}

// newBatchRetryHarness runs a batch with automatic publication delayed, so
// tests can drive each publish attempt synchronously in its event loop.
func newBatchRetryHarness(t *testing.T, restored,
	rejectPublish bool) *batchRetryHarness {

	t.Helper()
	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var wallet lndclient.WalletKitClient = lnd.WalletKit
	if rejectPublish {
		wallet = &testTransactionPublisher{WalletKitClient: wallet}
	}
	h := &batchRetryHarness{
		ctx:    ctx,
		wallet: &firstMinRelayFailure{WalletKitClient: wallet},
		helper: &signingFailureHelper{
			mockPresignedHelper: newMockPresignedHelper(),
		},
	}
	h.batcher = NewBatcher(
		h.wallet, lnd.ChainNotifier, lnd.Signer, testMuSig2SignSweep,
		testVerifySchnorrSig, lnd.ChainParams, NewStoreMock(), h.helper,
		WithPresignedHelper(h.helper),
		WithPublishErrorHandler(func(_ error, msg string, _ btclog.Logger) {
			h.logged = append(h.logged, msg)
		}),
	)
	first := h.newSweep(t, 1)
	cfg := h.batcher.newBatchConfig(defaultMaxTimeoutDistance)
	cfg.initialDelayProvider = zeroInitialDelay
	cfg.batchPublishDelay = time.Hour
	kit := h.batcher.newBatchKit()
	kit.log = batchPrefixLogger("retry test")
	if restored {
		kit.state = Open
		kit.primaryID = first.outpoint
		kit.sweeps = map[wire.OutPoint]sweep{first.outpoint: *first}
		kit.rbfCache.FeeRate = first.minFeeRate
		var err error
		h.batch, err = NewBatchFromDB(cfg, kit)
		require.NoError(t, err)
	} else {
		h.batch = NewBatch(cfg, kit)
		h.batch.setLog(kit.log)
	}
	_, err := h.batch.insertAndAcquireID(ctx)
	require.NoError(t, err)
	if restored {
		require.NoError(t, h.batch.persistSweep(ctx, *first, false))
	}

	// Drain the mock's unbuffered notification and publication channels.
	go func() {
		for {
			select {
			case <-lnd.RegisterSpendChannel:
			case <-lnd.RegisterConfChannel:
			case <-lnd.TxPublishChannel:
			case <-ctx.Done():
				return
			}
		}
	}()
	runErr := make(chan error, 1)
	go func() {
		runErr <- h.batch.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(test.Timeout):
			t.Error("batch did not stop after cancellation")
		}
	})
	if !restored {
		accepted, err := h.batch.addSweeps(ctx, []*sweep{first})
		require.NoError(t, err)
		require.True(t, accepted)
	}

	return h
}

func (h *batchRetryHarness) newSweep(t *testing.T, id byte) *sweep {
	t.Helper()
	op := wire.OutPoint{Hash: chainhash.Hash{id}, Index: 1}
	const value = btcutil.Amount(1_000_000)
	h.helper.SetOutpointOnline(op, true)
	require.NoError(t, h.batcher.PresignSweepsGroup(
		h.ctx, []Input{{Outpoint: op, Value: value}},
		sweepTimeout, destAddr, nil,
	))
	s, err := h.batcher.loadSweep(h.ctx, lntypes.Hash{id}, op, value)
	require.NoError(t, err)

	return s
}

func (h *batchRetryHarness) publish() error {
	var err error
	h.batch.testRunInEventLoop(h.ctx, func() {
		err = h.batch.publish(h.ctx)
	})

	return err
}

// TestBatchAdmissionRecovery checks that construction recovery reopens batch
// admission even if publication fails, and restored batches wait for that
// recovery before accepting new sweeps.
func TestBatchAdmissionRecovery(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, rejectPublish := range []bool{false, true} {
			name := fmt.Sprintf("restored=%v/reject_publish=%v",
				restored, rejectPublish)
			t.Run(name, func(t *testing.T) {
				h := newBatchRetryHarness(t, restored, rejectPublish)
				next := h.newSweep(t, 2)
				if restored {
					require.False(t, h.batch.matchesSweepMode(true))
					accepted, err := h.batch.addSweeps(
						h.ctx, []*sweep{next},
					)
					require.NoError(t, err)
					require.False(t, accepted)
				}

				h.wallet.fail.Store(true)
				require.NoError(t, h.publish())
				require.Equal(t, []string{"construct error"}, h.logged)
				require.False(t, h.batch.matchesSweepMode(true))
				accepted, err := h.batch.addSweeps(h.ctx, []*sweep{next})
				require.NoError(t, err)
				require.False(t, accepted)

				// The wallet recovers. Both a successful publish and
				// a rejected signed transaction must reopen admission.
				require.NoError(t, h.publish())
				if rejectPublish {
					require.Equal(t, "publish error", h.logged[1])
				}
				require.True(t, h.batch.matchesSweepMode(true))
				accepted, err = h.batch.addSweeps(h.ctx, []*sweep{next})
				require.NoError(t, err)
				require.True(t, accepted)
				require.Equal(t, 2, h.batch.numSweeps(h.ctx))
			})
		}
	}
}

// TestPresignedSigningTimeout checks that co-signer timeouts are retried
// without fee escalation, while unexpected signing failures stay fatal.
func TestPresignedSigningTimeout(t *testing.T) {
	unexpected := errors.New("unexpected signing failure")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "timeout", err: context.DeadlineExceeded},
		{
			name: "wrapped timeout",
			err:  fmt.Errorf("co-signer: %w", context.DeadlineExceeded),
		},
		{name: "unexpected signing failure", err: unexpected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBatchRetryHarness(t, true, false)
			var initialRate chainfee.SatPerKWeight
			h.batch.testRunInEventLoop(h.ctx, func() {
				h.helper.signErr = tc.err
				initialRate = h.batch.rbfCache.FeeRate
			})
			if errors.Is(tc.err, unexpected) {
				require.ErrorIs(t, h.publish(), unexpected)
				require.Equal(t, []string{"signing error"}, h.logged)
				return
			}

			for range 2 {
				require.NoError(t, h.publish())
				var currentRate chainfee.SatPerKWeight
				h.batch.testRunInEventLoop(h.ctx, func() {
					currentRate = h.batch.rbfCache.FeeRate
				})
				require.Equal(t, initialRate, currentRate)
			}
			require.Equal(t, []string{
				"signing timeout", "signing timeout",
			}, h.logged)

			h.batch.testRunInEventLoop(h.ctx, func() {
				h.helper.signErr = nil
			})
			require.NoError(t, h.publish())
			next := h.newSweep(t, 2)
			accepted, err := h.batch.addSweeps(h.ctx, []*sweep{next})
			require.NoError(t, err)
			require.True(t, accepted)
		})
	}
}
