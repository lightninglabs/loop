package loopin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestLegacyMappedLoopInRecovery runs the real keep-first migration before
// startup reconciliation. A failed older selector must not release a newer
// pending claim or an input for which the newer swap was already paid.
func TestLegacyMappedLoopInRecovery(t *testing.T) {
	for _, state := range []fsm.StateType{
		MonitorInvoiceAndHtlcTx, SucceededTransitioningFailed, Succeeded,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newOwnershipRecoveryTest(t)
			a := h.addSwap(t, Failed)
			b := h.addSwap(t, state)
			h.migrate(t, a.SwapHash)
			pending := h.pending(t)
			if state == MonitorInvoiceAndHtlcTx {
				require.Len(t, pending, 1)
				// The stale relation hides the deposit from B's join.
				require.Empty(t, pending[0].Deposits)
			}
			require.NoError(t, h.manager.reconcileLoopInDeposits(
				t.Context(), pending,
			))
			if state == MonitorInvoiceAndHtlcTx {
				require.Equal(t, deposit.LoopingIn, h.live.GetState())
				require.Equal(t, []lntypes.Hash{b.SwapHash}, h.deposits.restored)
				require.Empty(t, h.deposits.transitions)
			} else {
				require.Equal(t, deposit.LoopedIn, h.live.GetState())
				stored, err := h.depositStore.GetDeposit(t.Context(), h.live.ID)
				require.NoError(t, err)
				require.Equal(t, deposit.LoopedIn, stored.GetState())
			}
		})
	}
}

// TestLegacyMappedLoopInAmbiguousClaims fails closed when the stale mapping
// cannot distinguish two live selectors, including a paid terminal selector.
func TestLegacyMappedLoopInAmbiguousClaims(t *testing.T) {
	for _, otherState := range []fsm.StateType{
		MonitorInvoiceAndHtlcTx, SucceededTransitioningFailed, Succeeded,
	} {
		t.Run(string(otherState), func(t *testing.T) {
			h := newOwnershipRecoveryTest(t)
			a := h.addSwap(t, Failed)
			h.addSwap(t, MonitorInvoiceAndHtlcTx)
			h.addSwap(t, otherState)
			h.migrate(t, a.SwapHash)
			err := h.manager.reconcileLoopInDeposits(t.Context(), h.pending(t))
			require.ErrorContains(t, err, "ambiguous live owners")
			require.Equal(t, deposit.LoopingIn, h.live.GetState())
			require.Empty(t, h.deposits.transitions)
			require.Empty(t, h.deposits.restored)
		})
	}
}

// TestCurrentLoopInMappingWins preserves a modern retry's durable owner even
// when an older canceled swap still monitors the same original outpoint.
func TestCurrentLoopInMappingWins(t *testing.T) {
	h := newOwnershipRecoveryTest(t)
	h.addSwap(t, MonitorInvoiceAndHtlcTx)
	b := h.addSwap(t, MonitorInvoiceAndHtlcTx)
	h.load(t)
	require.NoError(t, h.manager.reconcileLoopInDeposits(
		t.Context(), h.pending(t),
	))
	require.Equal(t, deposit.LoopingIn, h.live.GetState())
	require.Equal(t, []lntypes.Hash{b.SwapHash}, h.deposits.restored)
	require.Empty(t, h.deposits.transitions)
}

// TestLegacyMappedCanonicalSnapshot matches a valid alternate hash encoding
// rather than mistaking its live claimant for an orphan.
func TestLegacyMappedCanonicalSnapshot(t *testing.T) {
	h := newOwnershipRecoveryTest(t)
	a := h.addSwap(t, Failed)
	b := h.addSwap(t, MonitorInvoiceAndHtlcTx)
	_, err := h.db.ExecContext(t.Context(),
		"UPDATE static_address_swaps SET deposit_outpoints = $1 "+
			"WHERE swap_hash = $2", strings.ToUpper(h.live.String()),
		b.SwapHash[:])
	require.NoError(t, err)
	h.migrate(t, a.SwapHash)
	require.NoError(t, h.manager.reconcileLoopInDeposits(
		t.Context(), h.pending(t),
	))
	require.Equal(t, deposit.LoopingIn, h.live.GetState())
	require.Equal(t, []lntypes.Hash{b.SwapHash}, h.deposits.restored)
}

type ownershipRecoveryTest struct {
	db           *loopdb.BaseDB
	depositStore *deposit.SqlStore
	store        *SqlStore
	manager      *Manager
	deposits     *ownershipRecoveryDeposits
	live         *deposit.Deposit
	nextSwap     byte
}

func newOwnershipRecoveryTest(t *testing.T) *ownershipRecoveryTest {
	t.Helper()
	db := loopdb.NewTestDB(t)
	t.Cleanup(func() { db.Close() })
	d := &deposit.Deposit{
		ID: deposit.ID{1}, Hash: chainhash.Hash{0xab}, Value: 100_000,
		TimeOutSweepPkScript: []byte{0x51},
	}
	d.SetState(deposit.LoopingIn)
	setPersistedTestDepositAddress(t, t.Context(), db.BaseDB, d)
	deposits := deposit.NewSqlStore(db.BaseDB)
	require.NoError(t, deposits.CreateDeposit(t.Context(), d))
	store := NewSqlStore(loopdb.NewTypedStore[Querier](db),
		clock.NewTestClock(time.Now()), &chaincfg.RegressionNetParams)
	return &ownershipRecoveryTest{
		db: db.BaseDB, depositStore: deposits, store: store, live: d,
	}
}

func (h *ownershipRecoveryTest) addSwap(t *testing.T,
	state fsm.StateType) *StaticAddressLoopIn {

	t.Helper()
	h.nextSwap++
	_, client := test.CreateKey(1)
	_, server := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)
	swap := &StaticAddressLoopIn{
		SwapHash:         lntypes.Hash{h.nextSwap},
		SwapPreimage:     lntypes.Preimage{h.nextSwap},
		InitiationTime:   time.Now(),
		DepositOutpoints: []string{h.live.String()},
		Deposits:         []*deposit.Deposit{h.live},
		ClientPubkey:     client, ServerPubkey: server,
		HtlcTimeoutSweepAddress: addr,
	}
	swap.SetState(state)
	require.NoError(t, h.store.CreateLoopIn(t.Context(), swap))
	return swap
}

func (h *ownershipRecoveryTest) migrate(t *testing.T, oldest lntypes.Hash) {
	t.Helper()
	require.NoError(t, MigrateDepositSwapHash(t.Context(),
		loopdb.NewStoreMock(t), h.depositStore, h.store))
	h.load(t)
	require.Equal(t, oldest, *h.live.SwapHash)
}

func (h *ownershipRecoveryTest) load(t *testing.T) {
	t.Helper()
	var err error
	h.live, err = h.depositStore.GetDeposit(t.Context(), h.live.ID)
	require.NoError(t, err)
	h.deposits = &ownershipRecoveryDeposits{
		recordingDepositManager: newRecordingDepositManager(h.live),
		store:                   h.depositStore,
	}
	h.manager = &Manager{cfg: &Config{
		Store: h.store, DepositManager: h.deposits,
	}}
}

func (h *ownershipRecoveryTest) pending(t *testing.T) []*StaticAddressLoopIn {
	t.Helper()
	swaps, err := h.store.GetStaticAddressLoopInSwapsByStates(
		t.Context(), PendingStates,
	)
	require.NoError(t, err)
	return swaps
}

type ownershipRecoveryDeposits struct {
	*recordingDepositManager

	store    *deposit.SqlStore
	restored []lntypes.Hash
}

func (m *ownershipRecoveryDeposits) GetActiveDepositsInState(
	state fsm.StateType) ([]*deposit.Deposit, error) {

	var deposits []*deposit.Deposit
	for _, d := range m.active {
		if d.IsInState(state) {
			deposits = append(deposits, d)
		}
	}
	return deposits, nil
}

func (m *ownershipRecoveryDeposits) RestoreLoopInClaim(_ context.Context,
	_ []*deposit.Deposit, hash lntypes.Hash) error {

	m.restored = append(m.restored, hash)
	return nil
}

func (m *ownershipRecoveryDeposits) TransitionDeposits(ctx context.Context,
	deposits []*deposit.Deposit, event fsm.EventType, state fsm.StateType) error {

	if err := m.recordingDepositManager.TransitionDeposits(
		ctx, deposits, event, state,
	); err != nil {
		return err
	}
	for _, d := range deposits {
		d.Lock()
		err := m.store.UpdateDeposit(ctx, d)
		d.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
