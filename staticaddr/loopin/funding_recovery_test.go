package loopin

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/stretchr/testify/require"
)

// TestReplacedFundingRecovery exercises SQL recovery and the real deposit
// manager when every selected unconfirmed original has disappeared from lnd.
func TestReplacedFundingRecovery(t *testing.T) {
	for _, test := range []struct {
		name       string
		decision   ConfirmationRiskDecision
		noSnapshot bool
		missing    bool
		depth      int32
		resolved   bool
	}{
		{name: "rejected single replaced deposit", decision: ConfirmationRiskDecisionRejected,
			depth: defaultConfTarget, resolved: true},
		{name: "canceled deadline after restart", decision: ConfirmationRiskDecisionAccepted,
			depth: defaultConfTarget, resolved: true},
		{name: "legacy without snapshot", decision: ConfirmationRiskDecisionRejected,
			noSnapshot: true, depth: defaultConfTarget},
		{name: "missing funding and htlc history", decision: ConfirmationRiskDecisionRejected,
			missing: true},
		{name: "mempool conflict", decision: ConfirmationRiskDecisionRejected},
		{name: "shallow conflict", decision: ConfirmationRiskDecisionRejected,
			depth: defaultConfTarget - 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := wire.NewMsgTx(2)
			parent.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{9},
			}})
			parent.AddTxOut(wire.NewTxOut(200_000, nil))
			h := newExpiryRecoveryHarnessWithFunding(
				t, deposit.LoopingIn, false, parent,
			)
			store := NewSqlStore(loopdb.NewTypedStore[Querier](h.db),
				clock.NewDefaultClock(), h.lnd.ChainParams)
			h.lnd.Transactions = []lndclient.Transaction{{Tx: parent}}
			if !test.noSnapshot {
				var err error
				h.f.loopIn.FundingTxs, err = fundingTransactions(
					h.ctx, h.lnd.Client, h.f.loopIn.Deposits,
				)
				require.NoError(t, err)
			}
			require.NoError(t, store.CreateLoopIn(h.ctx, h.f.loopIn))
			require.NoError(t, store.RecordStaticAddressRiskDecision(
				h.ctx, h.f.loopIn.SwapHash, test.decision,
			))

			// Reconstruct from persisted state with only the replacement in
			// wallet history. No transaction spends the vanished deposit.
			h.lnd.Transactions = nil
			loaded, err := store.GetStaticAddressLoopInSwapsByStates(
				h.ctx, PendingStates,
			)
			require.NoError(t, err)
			require.Len(t, loaded, 1)
			replacement := parent.Copy()
			replacement.TxOut[0].Value--
			wallet := &mockTxListLightningClient{listed: make(chan struct{}, 10)}
			if !test.missing {
				wallet.txs = []lndclient.Transaction{{Tx: replacement,
					Confirmations: test.depth}}
			}
			h.f.cfg.Store = store
			h.f.cfg.TxOutChecker = NewLndTxOutChecker(wallet)
			h.f, err = NewFSM(h.ctx, loaded[0], h.f.cfg, true)
			require.NoError(t, err)
			h.lnd.SetInvoice(&lndclient.Invoice{
				Hash: h.f.loopIn.SwapHash, State: invoices.ContractCanceled,
			})
			result := make(chan fsm.EventType, 1)
			completed := make(chan error, 1)
			monitorCtx, cancel := context.WithCancel(h.ctx)
			defer cancel()
			h.monitorResult = result
			go func() {
				completed <- h.f.SendEvent(monitorCtx, OnRecover, nil)
				result <- fsm.EventType(h.f.loopIn.GetState())
			}()
			expiryRecoveryReceive(t, h.ctx, h.notifier.confs)
			blocks := expiryRecoveryReceive(t, h.ctx, h.notifier.blocks)
			if test.resolved && test.decision == ConfirmationRiskDecisionAccepted {
				h.sendBlock(t, blocks, expiryRecoveryHeight)
			}
			if test.resolved {
				require.Equal(t, fsm.EventType(Failed),
					expiryRecoveryReceive(t, h.ctx, result))
				require.NoError(t, expiryRecoveryReceive(t, h.ctx, completed))
				h.requireStoredState(t, deposit.Deposited)
				stored, err := store.GetLoopInByHash(h.ctx, loaded[0].SwapHash)
				require.NoError(t, err)
				require.Equal(t, Failed, stored.GetState())
				return
			}
			expiryRecoveryReceive(t, h.ctx, wallet.listed)
			h.sendBlock(t, blocks, expiryRecoveryHeight)
			h.sendBlock(t, blocks, expiryRecoveryHeight+1)
			h.requireActive(t, deposit.LoopingIn)
			select {
			case state := <-result:
				t.Fatalf("swap unexpectedly finalized in %s", state)
			default:
			}
			cancel()
			require.NoError(t, expiryRecoveryReceive(t, h.ctx, completed))
		})
	}
}
