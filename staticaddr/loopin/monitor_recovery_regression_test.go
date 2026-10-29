package loopin

import (
	"context"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestRejectedFundingFinalizesAfterConflictDepth waits for positive conflict
// proof after an early rejection and fails the swap before its HTLC timeout.
func TestRejectedFundingFinalizesAfterConflictDepth(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "runtime rejection"
		if recovered {
			name = "restored rejection"
		}
		t.Run(name, func(t *testing.T) {
			funding := newMonitorRecoveryFunding()
			h := newExpiryRecoveryHarnessWithFunding(
				t, deposit.LoopingIn, false, funding,
			)
			h.f.loopIn.InitiationHeight = expiryRecoveryHeight
			h.f.loopIn.HtlcCltvExpiry = expiryRecoveryHeight + 1_000
			h.f.loopIn.FundingTxs = []*wire.MsgTx{funding.Copy()}
			store := NewSqlStore(loopdb.NewTypedStore[Querier](h.db),
				clock.NewDefaultClock(), h.lnd.ChainParams)
			require.NoError(t, store.CreateLoopIn(h.ctx, h.f.loopIn))
			if recovered {
				require.NoError(t, store.RecordStaticAddressRiskDecision(
					h.ctx, h.f.loopIn.SwapHash,
					ConfirmationRiskDecisionRejected,
				))
			}
			loaded, err := store.GetLoopInByHash(h.ctx, h.f.loopIn.SwapHash)
			require.NoError(t, err)
			h.f.cfg.Store = store
			h.f, err = NewFSM(h.ctx, loaded, h.f.cfg, true)
			require.NoError(t, err)

			replacement := funding.Copy()
			replacement.TxOut[0].Value--
			history := newMonitorRecoveryHistory()
			history.setTransactions([]lndclient.Transaction{{
				Tx: replacement, Confirmations: 1,
			}})
			h.f.cfg.TxOutChecker = NewLndTxOutChecker(history)
			notifications := &mockNotificationManager{
				riskAccepted: make(
					chan *swapserverrpc.ServerStaticLoopInRiskAcceptedNotification,
				),
				riskRejected: make(
					chan *swapserverrpc.ServerStaticLoopInRiskRejectedNotification, 1,
				),
			}
			h.f.cfg.NotificationManager = notifications
			invoiceState := invoices.ContractOpen
			if recovered {
				invoiceState = invoices.ContractCanceled
			}
			h.lnd.SetInvoice(&lndclient.Invoice{
				Hash: h.f.loopIn.SwapHash, State: invoiceState,
			})

			result := make(chan error, 1)
			go func() {
				result <- h.f.SendEvent(h.ctx, OnRecover, nil)
			}()
			expiryRecoveryReceive(t, h.ctx, h.notifier.confs)
			blocks := expiryRecoveryReceive(t, h.ctx, h.notifier.blocks)
			if !recovered {
				rejected := &swapserverrpc.ServerStaticLoopInRiskRejectedNotification{
					SwapHash: h.f.loopIn.SwapHash[:],
				}
				notifications.riskRejected <- rejected
				expiryRecoveryReceive(t, h.ctx, h.lnd.FailInvoiceChannel)
			}
			expiryRecoveryReceive(t, h.ctx, history.listed)
			h.requireActive(t, deposit.LoopingIn)

			// A stale acceptance must not undo the recorded rejection while
			// the replacement is still too shallow to prove a conflict.
			accepted := &swapserverrpc.ServerStaticLoopInRiskAcceptedNotification{
				SwapHash: h.f.loopIn.SwapHash[:],
			}
			// The watcher has one buffered update. Three unbuffered
			// handoffs ensure the monitor consumes an acceptance before
			// the next block can establish the conflict.
			for range 3 {
				select {
				case notifications.riskAccepted <- accepted:
				case <-h.ctx.Done():
					t.Fatal(h.ctx.Err())
				}
			}
			h.sendBlock(t, blocks, expiryRecoveryHeight+1)
			expiryRecoveryReceive(t, h.ctx, history.listed)
			h.requireActive(t, deposit.LoopingIn)

			history.setTransactions([]lndclient.Transaction{{
				Tx: replacement, Confirmations: defaultConfTarget,
			}})
			h.sendBlock(t, blocks, expiryRecoveryHeight+2)
			require.NoError(t, expiryRecoveryReceive(t, h.ctx, result))
			require.Equal(t, Failed, h.f.loopIn.GetState())
			h.requireStoredState(t, deposit.Deposited)
			stored, err := store.GetLoopInByHash(h.ctx, loaded.SwapHash)
			require.NoError(t, err)
			require.Equal(t, Failed, stored.GetState())
			require.Equal(t, ConfirmationRiskDecisionRejected,
				stored.ConfirmationRiskDecision)
		})
	}
}

// TestReorgedHtlcKeepsTimeoutRecovery preserves the timeout deposit and its
// monitor after the wallet forgets a disconnected HTLC, then refunds it when
// the HTLC confirms again. Caller copies must not hide the canonical state.
func TestReorgedHtlcKeepsTimeoutRecovery(t *testing.T) {
	for _, test := range []struct {
		name         string
		callerCopy   bool
		unknownOwner bool
	}{
		{name: "canonical deposits"},
		{name: "caller deposit copies", callerCopy: true},
		{name: "unknown recovered owner", unknownOwner: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			funding := newMonitorRecoveryFunding()
			h := newExpiryRecoveryHarnessWithFunding(
				t, deposit.LoopingIn, true, funding,
			)
			if test.callerCopy {
				copied, err := h.store.GetDeposit(h.ctx, h.dep.ID)
				require.NoError(t, err)
				h.f.loopIn.Deposits = []*deposit.Deposit{copied}
			}
			h.lnd.SetInvoice(&lndclient.Invoice{
				Hash: h.f.loopIn.SwapHash, State: invoices.ContractCanceled,
			})
			history := newMonitorRecoveryHistory()
			h.f.cfg.TxOutChecker = NewLndTxOutChecker(history)
			notifier := &monitorRecoveryReorgNotifier{
				expiryRecoveryNotifier: h.notifier,
				reorgs:                 make(chan chan struct{}, 4),
			}
			h.f.cfg.ChainNotifier = notifier
			htlc, err := h.f.loopIn.createHtlcTx(h.lnd.ChainParams,
				h.f.loopIn.HtlcTxFeeRate, 1)
			require.NoError(t, err)
			history.setTransactions([]lndclient.Transaction{
				{Tx: funding, Confirmations: 1_601},
				{Tx: htlc, Confirmations: defaultConfTarget},
			})
			result, conf, blocks := h.monitor(t)
			reorg := expiryRecoveryReceive(t, h.ctx, notifier.reorgs)
			h.sendConfirmation(t, conf, htlc, expiryRecoveryHeight-2)
			for {
				stored, err := h.store.GetDeposit(h.ctx, h.dep.ID)
				require.NoError(t, err)
				if stored.IsInState(deposit.SweepHtlcTimeout) {
					break
				}
				expiryRecoveryReceive(t, h.ctx, h.store.updates)
			}

			if test.unknownOwner {
				// Recovered timeout deposits have no in-memory loop-in claim.
				h.manager.ClearLoopInClaim(h.dep.OutPoint)
			}

			// A wallet may remove the disconnected HTLC after a failed
			// rebroadcast. The retained funding output is then unspent,
			// but releasing its timeout state would abandon refund recovery.
			reorg <- struct{}{}
			reconfirmation := expiryRecoveryReceive(t, h.ctx, h.notifier.confs)
			history.setTransactions([]lndclient.Transaction{{
				Tx: funding, Confirmations: 1_601,
			}})
			outputs, err := h.f.cfg.TxOutChecker.GetTxOuts(
				h.ctx, []wire.OutPoint{h.dep.OutPoint},
			)
			require.NoError(t, err)
			require.NotNil(t, outputs[h.dep.OutPoint])
			h.sendBlock(t, blocks, expiryRecoveryHeight)
			h.sendBlock(t, blocks, expiryRecoveryHeight+1)
			h.requireActive(t, deposit.SweepHtlcTimeout)
			h.requireStoredState(t, deposit.SweepHtlcTimeout)
			if test.callerCopy {
				require.Equal(t, deposit.LoopingIn,
					h.f.loopIn.Deposits[0].GetState())
			}

			h.sendConfirmation(t, reconfirmation, htlc, expiryRecoveryHeight-2)
			h.sendBlock(t, blocks, expiryRecoveryHeight+2)
			require.Equal(t, OnSweepHtlcTimeout,
				expiryRecoveryReceive(t, h.ctx, result))
			require.Equal(t, OnHtlcTimeoutSweepPublished,
				h.f.SweepHtlcTimeoutAction(h.ctx, nil))
			refund := expiryRecoveryReceive(t, h.ctx, h.wallet.published)
			require.Equal(t, htlc.TxHash(), refund.TxIn[0].PreviousOutPoint.Hash)
			finished := make(chan fsm.EventType, 1)
			go func() {
				finished <- h.f.MonitorHtlcTimeoutSweepAction(h.ctx, nil)
			}()
			refundConf := expiryRecoveryReceive(t, h.ctx, h.notifier.confs)
			h.sendConfirmation(t, refundConf, refund, expiryRecoveryHeight+3)
			require.Equal(t, OnHtlcTimeoutSwept,
				expiryRecoveryReceive(t, h.ctx, finished))
			h.requireStoredState(t, deposit.HtlcTimeoutSwept)
		})
	}
}

// TestReusedTimeoutDepositDoesNotKeepOldSwapPending lets an old canceled
// swap finish after another swap's HTLC confirms, without releasing the new
// owner's timeout claim or waiting for its refund.
func TestReusedTimeoutDepositDoesNotKeepOldSwapPending(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		name := "payment deadline"
		if rejected {
			name = "risk rejection"
		}
		t.Run(name, func(t *testing.T) {
			h := newExpiryRecoveryHarness(t, deposit.LoopingIn, false)
			a := h.f.loopIn.SwapHash
			b := lntypes.Hash{9}
			require.NoError(t, h.manager.ReleaseLoopInDeposits(
				h.ctx, h.f.loopIn.Deposits, a,
			))
			require.NoError(t, h.manager.LockDepositsForLoopIn(
				h.ctx, h.f.loopIn.Deposits, b,
			))
			require.NoError(t, h.manager.TransitionDeposits(
				h.ctx, h.f.loopIn.Deposits, deposit.OnSweepingHtlcTimeout,
				deposit.SweepHtlcTimeout,
			))
			spend := wire.NewMsgTx(2)
			spend.AddTxIn(&wire.TxIn{PreviousOutPoint: h.dep.OutPoint})
			spend.AddTxOut(wire.NewTxOut(199_000, []byte{0x51}))
			history := newMonitorRecoveryHistory()
			history.setTransactions([]lndclient.Transaction{{
				Tx: spend, Confirmations: defaultConfTarget,
			}})
			h.f.cfg.TxOutChecker = NewLndTxOutChecker(history)
			h.lnd.SetInvoice(&lndclient.Invoice{
				Hash: a, State: invoices.ContractCanceled,
			})
			if rejected {
				h.f.loopIn.ConfirmationRiskDecision =
					ConfirmationRiskDecisionRejected
				h.f.loopIn.HtlcCltvExpiry = expiryRecoveryHeight + 1_000
			}
			result, _, blocks := h.monitor(t)
			want := fsm.OnError
			if !rejected {
				h.sendBlock(t, blocks, expiryRecoveryHeight)
				want = OnSwapTimedOut
			}
			require.Equal(t, want,
				expiryRecoveryReceive(t, h.ctx, result))
			require.NoError(t, h.f.unlockDeposits(h.ctx))
			h.requireActive(t, deposit.SweepHtlcTimeout)
			h.requireStoredState(t, deposit.SweepHtlcTimeout)
			owner, known := h.manager.GetLoopInClaim(h.dep.OutPoint)
			require.True(t, known)
			require.Equal(t, b, owner)
		})
	}
}

// newMonitorRecoveryFunding creates a parent whose output the harness binds
// to the deposit's actual static-address script.
func newMonitorRecoveryFunding() *wire.MsgTx {
	funding := wire.NewMsgTx(2)
	funding.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
		Hash: chainhash.Hash{42},
	}})
	funding.AddTxOut(wire.NewTxOut(200_000, nil))
	return funding
}

// monitorRecoveryHistory provides immutable transaction snapshots to the
// production checker while the test changes wallet history between blocks.
type monitorRecoveryHistory struct {
	lndclient.LightningClient

	mu           sync.Mutex
	transactions []lndclient.Transaction
	listed       chan struct{}
}

// newMonitorRecoveryHistory records when a snapshot has been queried.
func newMonitorRecoveryHistory() *monitorRecoveryHistory {
	return &monitorRecoveryHistory{listed: make(chan struct{}, 16)}
}

// setTransactions replaces the snapshot; the caller leaves its contents intact.
func (h *monitorRecoveryHistory) setTransactions(txs []lndclient.Transaction) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.transactions = txs
}

// GetInfo supplies a stable synced tip around the checker's wallet query.
func (h *monitorRecoveryHistory) GetInfo(context.Context) (
	*lndclient.Info, error) {

	return &lndclient.Info{BlockHeight: expiryRecoveryHeight,
		BestBlockHash: chainhash.Hash{9}, SyncedToChain: true}, nil
}

// ListTransactions returns the current snapshot and records the query.
func (h *monitorRecoveryHistory) ListTransactions(context.Context, int32, int32,
	...lndclient.ListTransactionsOption) ([]lndclient.Transaction, error) {

	h.mu.Lock()
	transactions := h.transactions
	h.mu.Unlock()
	h.listed <- struct{}{}
	return transactions, nil
}

// monitorRecoveryReorgNotifier exposes the actual HTLC reorg channel.
type monitorRecoveryReorgNotifier struct {
	*expiryRecoveryNotifier

	reorgs chan chan struct{}
}

// RegisterConfirmationsNtfn retains the harness confirmation behavior and
// forwards the caller's reorg channel to the test.
func (n *monitorRecoveryReorgNotifier) RegisterConfirmationsNtfn(
	ctx context.Context, hash *chainhash.Hash, script []byte, confs, height int32,
	options ...lndclient.NotifierOption) (
	chan *chainntnfs.TxConfirmation, chan error, error) {

	opts := lndclient.DefaultNotifierOptions()
	for _, option := range options {
		option(opts)
	}
	n.reorgs <- opts.ReOrgChan
	return n.expiryRecoveryNotifier.RegisterConfirmationsNtfn(
		ctx, hash, script, confs, height, options...,
	)
}
