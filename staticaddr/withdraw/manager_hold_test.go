package withdraw

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/lndclient"
	fsmv2 "github.com/lightninglabs/loop/fsm/v2"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/stretchr/testify/require"
)

func init() {
	UseLogger(btclog.Disabled)
}

type blockedPublisher struct {
	lndclient.WalletKitClient

	entered chan *wire.MsgTx
	release chan error
}

func (w *blockedPublisher) PublishTransaction(ctx context.Context,
	tx *wire.MsgTx, _ string) error {

	select {
	case w.entered <- tx:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-w.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The wallet can remove a spent input while PublishTransaction is still
// blocked. The signed candidate and claim must already be durable and the
// deposit must remain active even if the publisher gets no acknowledgement.
func TestWithdrawalHoldPreservesOrdering(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "published"},
		{name: "rejected", err: chain.ErrInsufficientFee},
		{name: "lost acknowledgement", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPersistenceFixture(t, 1)
			d := f.inputs[0]
			address := &sharedInputAddress{input: d}
			deposits, _ := startDepositManager(t, f.deposits, address)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			wallet := &blockedPublisher{
				entered: make(chan *wire.MsgTx, 1),
				release: make(chan error, 1),
			}
			m, err := NewManager(&ManagerConfig{
				WalletKit: wallet, DepositManager: deposits, Store: f.store,
			}, 600)
			require.NoError(t, err)
			m.lifecycle = ctx
			t.Cleanup(func() { cancel(); m.wg.Wait() })
			data, signed := f.intent(t, f.inputs, 103)
			raw, err := encodeWorkflowTx(signed)
			require.NoError(t, err)

			cfg := graphConfig(f, data)
			cfg.Store = &ownedWorkflowStore{manager: m, inner: f.store.WorkflowStore()}
			pending := cfg.States[WorkflowPublishPending]
			pending.Enter = func(_ context.Context, view fsmv2.Snapshot[WorkflowData]) error {
				m.ensurePublisher(view)
				return nil
			}
			cfg.States[WorkflowPublishPending] = pending
			machine, err := fsmv2.Create(ctx, cfg, WorkflowSigning, data)
			require.NoError(t, err)
			m.workflows[data.ID] = &withdrawalRuntime{machine: machine}
			require.NoError(t, machine.Recover(ctx))
			_, err = machine.Send(ctx, workflowSigned, workflowEvent{Attempt: 1, Tx: raw})
			require.NoError(t, err)
			select {
			case tx := <-wallet.entered:
				require.Equal(t, signed.TxHash(), tx.TxHash())
			case <-ctx.Done():
				t.Fatal("publication did not start")
			}

			address.spent.Store(true)
			require.NoError(t, deposits.EnsureDepositsFresh(ctx))
			assertClaim := func() {
				t.Helper()
				selected, active := deposits.AllOutpointsActiveDeposits(
					[]wire.OutPoint{d.OutPoint}, deposit.Withdrawing,
				)
				require.True(t, active)
				require.Equal(t, signed.TxHash(), selected[0].FinalizedWithdrawalTx.TxHash())
				owner, err := f.db.GetWithdrawalWorkflowClaim(ctx, d.ID[:])
				require.NoError(t, err)
				require.Equal(t, data.ID, owner)
			}
			assertClaim()

			wallet.release <- tc.err
			select {
			case result := <-m.results:
				require.ErrorIs(t, result.err, tc.err)
				m.handleWorkflowResult(ctx, result)
			case <-ctx.Done():
				t.Fatal("publication result did not arrive")
			}
			expected := WorkflowMonitoring
			if tc.err != nil {
				expected = WorkflowPublishPending
			}
			record, err := cfg.Store.Load(ctx, data.ID)
			require.NoError(t, err)
			require.Equal(t, expected, record.State)
			assertClaim()
		})
	}
}
