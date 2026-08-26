package deposit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

type expiryReorgSpendWatch struct {
	ctx      context.Context
	outpoint wire.OutPoint
	script   []byte
	height   int32
	reorgs   chan struct{}
	spends   chan *chainntnfs.SpendDetail
	errors   chan error
}

type expiryReorgConfWatch struct {
	ctx     context.Context
	txid    chainhash.Hash
	script  []byte
	confs   int32
	height  int32
	updates chan *chainntnfs.TxConfirmation
	errors  chan error
	release chan struct{}
}

// expiryReorgNotifier exposes each subscription so tests can deliver ordered
// chain events and observe when obsolete confirmation watches are canceled.
type expiryReorgNotifier struct {
	lndclient.ChainNotifierClient

	spends chan expiryReorgSpendWatch
	confs  chan expiryReorgConfWatch
}

func (n *expiryReorgNotifier) RegisterSpendNtfn(ctx context.Context,
	outpoint *wire.OutPoint, script []byte, height int32,
	options ...lndclient.NotifierOption) (chan *chainntnfs.SpendDetail,
	chan error, error) {

	opts := lndclient.DefaultNotifierOptions()
	for _, option := range options {
		option(opts)
	}
	watch := expiryReorgSpendWatch{
		ctx: ctx, outpoint: *outpoint, script: script, height: height,
		reorgs: opts.ReOrgChan,
		spends: make(chan *chainntnfs.SpendDetail, 1),
		errors: make(chan error, 1),
	}
	n.spends <- watch
	return watch.spends, watch.errors, nil
}

func (n *expiryReorgNotifier) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, script []byte, confs, height int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	watch := expiryReorgConfWatch{
		ctx: ctx, txid: *txid, script: script, confs: confs, height: height,
		updates: make(chan *chainntnfs.TxConfirmation, 1),
		errors:  make(chan error, 1),
		release: make(chan struct{}),
	}
	n.confs <- watch
	select {
	case <-watch.release:
		return watch.updates, watch.errors, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

type expiryReorgHarness struct {
	t        *testing.T
	ctx      context.Context
	cancel   context.CancelFunc
	fsm      *FSM
	notifier *expiryReorgNotifier
	spend    expiryReorgSpendWatch
	result   chan fsm.EventType
}

func newExpiryReorgHarness(t *testing.T) *expiryReorgHarness {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	notifier := &expiryReorgNotifier{
		spends: make(chan expiryReorgSpendWatch, 1),
		confs:  make(chan expiryReorgConfWatch, 1),
	}
	h := &expiryReorgHarness{
		t: t, ctx: ctx, cancel: cancel, notifier: notifier,
		result: make(chan fsm.EventType, 1),
		fsm: &FSM{
			StateMachine: &fsm.StateMachine{},
			cfg:          &ManagerConfig{ChainNotifier: notifier},
			deposit: &Deposit{
				Hash:                 chainhash.Hash{1},
				Index:                2,
				ConfirmationHeight:   42,
				ExpirySweepTxid:      chainhash.Hash{9},
				TimeOutSweepPkScript: []byte{0x51, 0x20, 0x01},
				AddressParams: &script.Parameters{
					PkScript: []byte{0x51, 0x20, 0x00},
				},
			},
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.result <- h.fsm.WaitForExpirySweepAction(ctx, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("expiry action did not stop after cancellation")
		}
	})
	h.receiveSpendWatch()
	return h
}

func (h *expiryReorgHarness) receiveSpendWatch() {
	h.t.Helper()
	select {
	case h.spend = <-h.notifier.spends:
	case event := <-h.result:
		h.t.Fatalf("expiry action returned before renewing spend watch: %v", event)
	case <-h.ctx.Done():
		h.t.Fatal("expiry spend subscription was not registered")
	}
	require.Equal(h.t, h.fsm.deposit.OutPoint, h.spend.outpoint)
	require.Equal(h.t, h.fsm.deposit.AddressParams.PkScript, h.spend.script)
	require.EqualValues(h.t, 42, h.spend.height)
	require.NotNil(h.t, h.spend.reorgs, "spend subscription must follow reorgs")
	require.Zero(h.t, cap(h.spend.reorgs), "reorg delivery must be synchronous")
}

func (h *expiryReorgHarness) transaction(lockTime uint32) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.LockTime = lockTime
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: h.fsm.deposit.OutPoint})
	tx.AddTxOut(&wire.TxOut{
		Value: 1000, PkScript: h.fsm.deposit.TimeOutSweepPkScript,
	})
	return tx
}

func (h *expiryReorgHarness) observeSpend(tx *wire.MsgTx,
	height int32) expiryReorgConfWatch {

	h.t.Helper()
	watch := h.prepareSpend(tx, height)
	close(watch.release)
	return watch
}

func (h *expiryReorgHarness) prepareSpend(tx *wire.MsgTx,
	height int32) expiryReorgConfWatch {

	h.t.Helper()
	h.spend.spends <- &chainntnfs.SpendDetail{
		SpendingTx: tx, SpendingHeight: height,
	}
	return h.receiveConfirmationWatch(tx, height)
}

func (h *expiryReorgHarness) receiveConfirmationWatch(tx *wire.MsgTx,
	height int32) expiryReorgConfWatch {

	h.t.Helper()
	var watch expiryReorgConfWatch
	select {
	case watch = <-h.notifier.confs:
	case event := <-h.result:
		h.t.Fatalf("expiry action returned before the next spend: %v", event)
	case <-h.ctx.Done():
		h.t.Fatal("expiry confirmation subscription was not registered")
	}
	require.Equal(h.t, tx.TxHash(), watch.txid)
	require.Equal(h.t, h.fsm.deposit.TimeOutSweepPkScript, watch.script)
	require.EqualValues(h.t, DefaultConfTarget, watch.confs)
	require.Equal(h.t, height, watch.height)
	return watch
}

func (h *expiryReorgHarness) reorg(
	watch expiryReorgConfWatch) expiryReorgSpendWatch {

	h.t.Helper()
	oldSpend := h.spend
	select {
	case oldSpend.reorgs <- struct{}{}:
	case <-h.ctx.Done():
		h.t.Fatal("expiry action did not receive the spend reorg")
	}
	select {
	case <-watch.ctx.Done():
	case <-h.ctx.Done():
		h.t.Fatal("reorg did not cancel the obsolete confirmation watch")
	}
	select {
	case <-oldSpend.ctx.Done():
	case <-h.ctx.Done():
		h.t.Fatal("reorg did not cancel the obsolete spend watch")
	}
	h.receiveSpendWatch()
	require.NoError(h.t, h.spend.ctx.Err())
	return oldSpend
}

func (h *expiryReorgHarness) outcome() fsm.EventType {
	h.t.Helper()
	select {
	case event := <-h.result:
		require.ErrorIs(h.t, h.spend.ctx.Err(), context.Canceled)
		return event
	case <-time.After(5 * time.Second):
		h.t.Fatal("expiry action did not return")
		return ""
	}
}

// TestWaitForExpirySweepActionReorg follows replacement spends and re-mined
// transactions, ignoring late results from every invalidated confirmation watch.
func TestWaitForExpirySweepActionReorg(t *testing.T) {
	for _, test := range []struct {
		name      string
		lockTimes []uint32
	}{
		{name: "replacement", lockTimes: []uint32{1, 2}},
		{name: "same spender re-mined", lockTimes: []uint32{1, 1}},
		{name: "repeated reorgs", lockTimes: []uint32{1, 2, 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newExpiryReorgHarness(t)
			var watches []expiryReorgConfWatch
			var retiredSpends []expiryReorgSpendWatch
			var tx *wire.MsgTx
			for i, lockTime := range test.lockTimes {
				tx = h.transaction(lockTime)
				watch := h.observeSpend(tx, int32(50+i))
				watches = append(watches, watch)
				if i == len(test.lockTimes)-1 {
					watch.updates <- &chainntnfs.TxConfirmation{Tx: tx}
					break
				}
				oldSpend := h.reorg(watch)
				retiredSpends = append(retiredSpends, oldSpend)
				oldSpend.spends <- &chainntnfs.SpendDetail{SpendingTx: tx}
				oldSpend.errors <- errors.New("obsolete spend watch failed")
				watch.updates <- &chainntnfs.TxConfirmation{Tx: tx}
				watch.errors <- errors.New("obsolete subscription failed")
			}
			require.Equal(t, OnExpirySwept, h.outcome())
			require.Equal(t, tx.TxHash(), h.fsm.deposit.ExpirySweepTxid)
			require.NoError(t, h.ctx.Err(), "action must cancel its own watches")
			for i, watch := range watches {
				require.ErrorIs(t, watch.ctx.Err(), context.Canceled)
				if i < len(watches)-1 {
					require.Len(t, watch.updates, 1)
					require.Len(t, watch.errors, 1)
				}
			}
			for _, watch := range retiredSpends {
				require.ErrorIs(t, watch.ctx.Err(), context.Canceled)
				require.Len(t, watch.spends, 1)
				require.Len(t, watch.errors, 1)
			}
		})
	}
}

// TestWaitForExpirySweepActionPendingReorg makes the old confirmation and its
// spend reorg ready together before the action resumes its notification loop.
func TestWaitForExpirySweepActionPendingReorg(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newExpiryReorgHarness(t)
		oldSpend := h.spend
		oldTx := h.transaction(1)
		oldConf := h.prepareSpend(oldTx, 50)
		oldConf.updates <- &chainntnfs.TxConfirmation{Tx: oldTx}
		go func() {
			select {
			case oldSpend.reorgs <- struct{}{}:
			case <-h.ctx.Done():
			}
		}()

		// Both senders are now blocked: the action inside registration and
		// the notifier delivering the reorg. Neither event can be missed.
		synctest.Wait()
		close(oldConf.release)
		h.receiveSpendWatch()
		require.ErrorIs(t, oldSpend.ctx.Err(), context.Canceled)
		require.ErrorIs(t, oldConf.ctx.Err(), context.Canceled)

		tx := h.transaction(2)
		conf := h.observeSpend(tx, 51)
		conf.updates <- &chainntnfs.TxConfirmation{Tx: tx}
		require.Equal(t, OnExpirySwept, h.outcome())
		require.Equal(t, tx.TxHash(), h.fsm.deposit.ExpirySweepTxid)
		require.ErrorIs(t, conf.ctx.Err(), context.Canceled)
		require.NoError(t, h.ctx.Err())
	})
}

// TestWaitForExpirySweepActionCancel cancels both phases of expiry monitoring
// and checks that no subscription outlives the action.
func TestWaitForExpirySweepActionCancel(t *testing.T) {
	for _, waitingForConfirmation := range []bool{false, true} {
		name := "waiting for spend"
		if waitingForConfirmation {
			name = "waiting for confirmation"
		}
		t.Run(name, func(t *testing.T) {
			h := newExpiryReorgHarness(t)
			var watch expiryReorgConfWatch
			if waitingForConfirmation {
				watch = h.observeSpend(h.transaction(1), 50)
			}
			h.cancel()
			require.Equal(t, fsm.OnError, h.outcome())
			require.Equal(t, chainhash.Hash{9}, h.fsm.deposit.ExpirySweepTxid)
			if waitingForConfirmation {
				require.ErrorIs(t, watch.ctx.Err(), context.Canceled)
			}
		})
	}
}

// TestWaitForExpirySweepActionReorgRejectsInvalidReplacement ensures each new
// spender must still spend this deposit and pay its configured sweep address.
func TestWaitForExpirySweepActionReorgRejectsInvalidReplacement(t *testing.T) {
	for _, wrongInput := range []bool{false, true} {
		name := "wrong destination"
		if wrongInput {
			name = "wrong input"
		}
		t.Run(name, func(t *testing.T) {
			h := newExpiryReorgHarness(t)
			h.reorg(h.observeSpend(h.transaction(1), 50))
			tx := h.transaction(2)
			if wrongInput {
				tx.TxIn[0].PreviousOutPoint.Index++
			} else {
				tx.TxOut[0].PkScript = []byte{0x51}
			}
			h.spend.spends <- &chainntnfs.SpendDetail{SpendingTx: tx}
			require.Equal(t, fsm.OnError, h.outcome())
			require.Equal(t, chainhash.Hash{9}, h.fsm.deposit.ExpirySweepTxid)
			require.Empty(t, h.notifier.confs)
		})
	}
}

// TestWaitForExpirySweepActionSpendStreamEnd distinguishes normal stream
// completion from failures without abandoning an active confirmation watch.
func TestWaitForExpirySweepActionSpendStreamEnd(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		spend     bool
		wantSwept bool
	}{
		{name: "EOF without spend", err: io.EOF},
		{
			name: "EOF with pending confirmation", err: io.EOF,
			spend: true, wantSwept: true,
		},
		{
			name:  "wrapped EOF with pending confirmation",
			err:   fmt.Errorf("stream ended: %w", io.EOF),
			spend: true, wantSwept: true,
		},
		{
			name: "error with pending confirmation",
			err:  errors.New("spend subscription failed"), spend: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newExpiryReorgHarness(t)
				tx := h.transaction(1)
				var watch expiryReorgConfWatch
				if test.spend {
					watch = h.observeSpend(tx, 50)
				}
				h.spend.errors <- test.err
				synctest.Wait()

				event := fsm.OnError
				if test.wantSwept {
					require.Empty(t, h.result)
					require.NoError(t, watch.ctx.Err())
					watch.updates <- &chainntnfs.TxConfirmation{Tx: tx}
					event = OnExpirySwept
				}
				require.Equal(t, event, h.outcome())
				if test.wantSwept {
					require.Equal(t, tx.TxHash(), h.fsm.deposit.ExpirySweepTxid)
				} else {
					require.Equal(t, chainhash.Hash{9}, h.fsm.deposit.ExpirySweepTxid)
				}
				if test.spend {
					require.ErrorIs(t, watch.ctx.Err(), context.Canceled)
				}
			})
		})
	}
}

// TestWaitForExpirySweepActionQueuedSpendBeforeEOF preserves a buffered spend
// when normal stream completion races with registering its confirmation watch.
func TestWaitForExpirySweepActionQueuedSpendBeforeEOF(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newExpiryReorgHarness(t)
		tx := h.transaction(1)
		h.spend.spends <- &chainntnfs.SpendDetail{
			SpendingTx: tx, SpendingHeight: 50,
		}
		h.spend.errors <- io.EOF
		watch := h.receiveConfirmationWatch(tx, 50)
		close(watch.release)
		synctest.Wait()
		require.Empty(t, h.result)
		require.NoError(t, watch.ctx.Err())
		watch.updates <- &chainntnfs.TxConfirmation{Tx: tx}
		require.Equal(t, OnExpirySwept, h.outcome())
		require.Equal(t, tx.TxHash(), h.fsm.deposit.ExpirySweepTxid)
		require.ErrorIs(t, watch.ctx.Err(), context.Canceled)
	})
}
