package loopin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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

// TestLegacyMappedLoopInAmbiguousClaims keeps unresolved inputs locked without
// preventing startup or suppressing the pending swaps' HTLC monitors.
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
			require.NoError(t, h.manager.reconcileLoopInDeposits(
				t.Context(), h.pending(t),
			))
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
	state fsm.StateType, deposits ...*deposit.Deposit) *StaticAddressLoopIn {

	t.Helper()
	if len(deposits) == 0 {
		deposits = []*deposit.Deposit{h.live}
	}
	outpoints := make([]string, 0, len(deposits))
	for _, d := range deposits {
		outpoints = append(outpoints, d.String())
	}
	h.nextSwap++
	_, client := test.CreateKey(1)
	_, server := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)
	swap := &StaticAddressLoopIn{
		SwapHash:         lntypes.Hash{h.nextSwap},
		SwapPreimage:     lntypes.Preimage{h.nextSwap},
		InitiationTime:   time.Now(),
		DepositOutpoints: outpoints,
		Deposits:         deposits,
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
	deposits, err := h.depositStore.AllDeposits(t.Context())
	require.NoError(t, err)
	for _, d := range deposits {
		if d.ID == h.live.ID {
			h.live = d
			break
		}
	}
	h.deposits = &ownershipRecoveryDeposits{
		recordingDepositManager: newRecordingDepositManager(deposits...),
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
		current := m.active[d.OutPoint]
		current.Lock()
		err := m.store.UpdateDeposit(ctx, current)
		current.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// TestNewerMappedOwnerExcludesReleasedClaims reproduces a normal database
// where older swaps still monitor canceled invoices after releasing the same
// deposit. A crash during a later initialization must not make them its owners.
func TestNewerMappedOwnerExcludesReleasedClaims(t *testing.T) {
	h := newOwnershipRecoveryTest(t)
	h.addSwap(t, MonitorInvoiceAndHtlcTx)
	h.addSwap(t, MonitorInvoiceAndHtlcTx)
	newer := h.addSwap(t, Failed)
	h.load(t)
	require.Equal(t, newer.SwapHash, *h.live.SwapHash)

	owner, err := h.store.GetLoopInByHash(t.Context(), newer.SwapHash)
	require.NoError(t, err)
	pending := h.pending(t)
	require.Len(t, pending, 2)
	for _, previous := range pending {
		require.Positive(t, previous.ID)
		require.Less(t, previous.ID, owner.ID)
	}

	require.NoError(t, h.manager.reconcileLoopInDeposits(
		t.Context(), pending,
	))
	require.Equal(t, deposit.Deposited, h.live.GetState())
	require.Empty(t, h.deposits.restored)
	stored, err := h.depositStore.GetDeposit(t.Context(), h.live.ID)
	require.NoError(t, err)
	require.Equal(t, deposit.Deposited, stored.GetState())
}

// TestRestoredLoopInIncludesEveryDeposit restores a legacy input that is
// missing from a partially mapped swap, retaining the original signing order
// and including that input in both release and paid finalization.
func TestRestoredLoopInIncludesEveryDeposit(t *testing.T) {
	for _, paid := range []bool{false, true} {
		name := "release"
		if paid {
			name = "paid"
		}
		t.Run(name, func(t *testing.T) {
			h := newOwnershipRecoveryTest(t)
			older := h.addSwap(t, Failed)
			sibling := &deposit.Deposit{
				ID: deposit.ID{2}, Hash: chainhash.Hash{0xcd},
				Value: 150_000, TimeOutSweepPkScript: []byte{0x51},
				AddressParams: h.live.AddressParams,
			}
			sibling.SetState(deposit.LoopingIn)
			require.NoError(t, h.depositStore.CreateDeposit(
				t.Context(), sibling,
			))
			newer := h.addSwap(t, MonitorInvoiceAndHtlcTx,
				h.live, sibling)
			h.migrate(t, older.SwapHash)
			pending := h.pending(t)
			require.Len(t, pending, 1)
			recovered := pending[0]
			require.Equal(t, newer.SwapHash, recovered.SwapHash)
			require.Len(t, recovered.Deposits, 1)
			require.Equal(t, sibling.ID, recovered.Deposits[0].ID)

			require.NoError(t, h.manager.reconcileLoopInDeposits(
				t.Context(), pending,
			))
			require.Len(t, recovered.Deposits, 2)
			require.Equal(t, h.live.ID, recovered.Deposits[0].ID)
			require.Equal(t, sibling.ID, recovered.Deposits[1].ID)
			require.Same(t, h.live, recovered.Deposits[0])
			active, allActive := h.manager.activeDepositsForLoopIn(recovered)
			require.True(t, allActive)
			recovered.Deposits = active

			f := &FSM{cfg: h.manager.cfg, loopIn: recovered}
			target := deposit.Deposited
			if paid {
				require.Equal(t, OnSucceeded,
					f.PaymentReceivedAction(t.Context(), nil))
				target = deposit.LoopedIn
			} else {
				require.NoError(t, f.unlockDeposits(t.Context()))
			}
			for _, d := range recovered.Deposits {
				stored, err := h.depositStore.GetDeposit(t.Context(), d.ID)
				require.NoError(t, err)
				require.Equal(t, target, stored.GetState())
			}
		})
	}
}

// TestRecoveryClaimIDs uses the same persistent ordering for normal swap reads
// and the compact terminal-claim lookup.
func TestRecoveryClaimIDs(t *testing.T) {
	h := newOwnershipRecoveryTest(t)
	older := h.addSwap(t, Failed)
	paid := h.addSwap(t, Succeeded)
	oldSwap, err := h.store.GetLoopInByHash(t.Context(), older.SwapHash)
	require.NoError(t, err)
	paidSwap, err := h.store.GetLoopInByHash(t.Context(), paid.SwapHash)
	require.NoError(t, err)
	require.Positive(t, oldSwap.ID)
	require.Greater(t, paidSwap.ID, oldSwap.ID)
	claims, err := h.store.GetPaidStaticAddressLoopInClaims(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.Equal(t, paidSwap.ID, claims[0].ID)
	require.Equal(t, paid.SwapHash, claims[0].SwapHash)
	claims, err = h.store.GetPaidStaticAddressLoopInClaims(
		t.Context(), paidSwap.ID,
	)
	require.NoError(t, err)
	require.Empty(t, claims)
}

func (m *ownershipRecoveryDeposits) ReleaseLoopInDeposits(ctx context.Context,
	deposits []*deposit.Deposit, _ lntypes.Hash) error {

	return m.TransitionDeposits(ctx, deposits, fsm.OnError, deposit.Deposited)
}

func (m *ownershipRecoveryDeposits) AllStringOutpointsActiveDeposits(
	outpoints []string, state fsm.StateType) ([]*deposit.Deposit, bool) {

	deposits := make([]*deposit.Deposit, 0, len(outpoints))
	for _, encoded := range outpoints {
		outpoint, err := wire.NewOutPointFromString(encoded)
		if err != nil {
			return nil, false
		}
		d, ok := m.active[*outpoint]
		if !ok || (state != fsm.EmptyState && !d.IsInState(state)) {
			return nil, false
		}
		deposits = append(deposits, d)
	}
	return deposits, true
}

// TestLegacyPendingMappedLoopInRecovery covers the old payment-deadline path:
// A released its deposit but kept monitoring its canceled invoice. The legacy
// migration must not let A's cleanup release B's pending or already-paid input.
func TestLegacyPendingMappedLoopInRecovery(t *testing.T) {
	for _, state := range []fsm.StateType{
		MonitorInvoiceAndHtlcTx, PaymentReceived,
		SucceededTransitioningFailed, Succeeded,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newOwnershipRecoveryTest(t)
			a := h.addSwap(t, MonitorInvoiceAndHtlcTx)
			b := h.addSwap(t, state)
			h.migrate(t, a.SwapHash)
			manager := h.useRealDepositManager(t)
			pending := h.pending(t)
			require.NoError(t, h.manager.reconcileLoopInDeposits(
				t.Context(), pending,
			))
			want := deposit.LoopedIn
			if state == MonitorInvoiceAndHtlcTx {
				want = deposit.LoopingIn
				require.Len(t, pending, 2)
				for _, swap := range pending {
					if swap.SwapHash == b.SwapHash {
						require.Len(t, swap.Deposits, 1)
						require.Same(t, h.live, swap.Deposits[0])
					}
				}
			}
			require.NoError(t, manager.ReleaseLoopInDeposits(
				t.Context(), []*deposit.Deposit{h.live}, a.SwapHash,
			))
			require.Equal(t, want, h.live.GetState())
			stored, err := h.depositStore.GetDeposit(t.Context(), h.live.ID)
			require.NoError(t, err)
			require.Equal(t, want, stored.GetState())
			if state == MonitorInvoiceAndHtlcTx {
				require.NoError(t, manager.ReleaseLoopInDeposits(
					t.Context(), []*deposit.Deposit{h.live}, b.SwapHash,
				))
				require.Equal(t, deposit.Deposited, h.live.GetState())
			}
		})
	}
}

// TestAmbiguousPendingMappingBlocksAllCleanup keeps the deposit locked for
// manual resolution while all pending swaps remain available for recovery.
func TestAmbiguousPendingMappingBlocksAllCleanup(t *testing.T) {
	h := newOwnershipRecoveryTest(t)
	a := h.addSwap(t, MonitorInvoiceAndHtlcTx)
	h.addSwap(t, MonitorInvoiceAndHtlcTx)
	h.addSwap(t, MonitorInvoiceAndHtlcTx)
	h.migrate(t, a.SwapHash)
	manager := h.useRealDepositManager(t)
	pending := h.pending(t)
	require.Len(t, pending, 3)
	require.NoError(t, h.manager.reconcileLoopInDeposits(t.Context(), pending))
	for _, swap := range pending {
		require.NoError(t, manager.ReleaseLoopInDeposits(
			t.Context(), []*deposit.Deposit{h.live}, swap.SwapHash,
		))
	}
	require.Equal(t, deposit.LoopingIn, h.live.GetState())
	stored, err := h.depositStore.GetDeposit(t.Context(), h.live.ID)
	require.NoError(t, err)
	require.Equal(t, deposit.LoopingIn, stored.GetState())
	require.Len(t, pending, 3)
}

// TestMappedPaidOwnerCannotBeDowngraded preserves settled ownership even when
// a newer pending snapshot also refers to the same original deposit.
func TestMappedPaidOwnerCannotBeDowngraded(t *testing.T) {
	for _, state := range []fsm.StateType{
		PaymentReceived, SucceededTransitioningFailed, Succeeded,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newOwnershipRecoveryTest(t)
			a := h.addSwap(t, state)
			b := h.addSwap(t, MonitorInvoiceAndHtlcTx)
			h.migrate(t, a.SwapHash)
			manager := h.useRealDepositManager(t)
			require.NoError(t, h.manager.reconcileLoopInDeposits(
				t.Context(), h.pending(t),
			))
			require.NoError(t, manager.ReleaseLoopInDeposits(
				t.Context(), []*deposit.Deposit{h.live}, b.SwapHash,
			))
			require.Equal(t, deposit.LoopedIn, h.live.GetState())
		})
	}
}

// useRealDepositManager exercises startup claim restoration and delayed cleanup
// with the production deposit manager, backed by the test's real SQL store.
func (h *ownershipRecoveryTest) useRealDepositManager(
	t *testing.T) *deposit.Manager {

	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	lnd := test.NewMockLnd()
	notifier := &expiryRecoveryNotifier{
		blocks: make(chan chan int32, 4),
		confs:  make(chan *expiryRecoveryConf, 4),
		spends: make(chan *expiryRecoverySpend, 4),
	}
	manager := deposit.NewManager(&deposit.ManagerConfig{
		LightningClient: &expiryRecoveryClient{LightningClient: lnd.Client},
		AddressManager:  &expiryRecoveryAddress{},
		Store:           h.depositStore,
		WalletKit: &expiryRecoveryWallet{
			WalletKitClient: lnd.WalletKit,
		},
		ChainNotifier: notifier, Signer: &expiryRecoverySigner{},
	})
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- manager.Run(ctx, ready) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(testTimeout):
			t.Error("deposit manager did not stop")
		}
	})
	expiryRecoveryReceive(t, ctx, notifier.blocks)
	expiryRecoveryReceive(t, ctx, ready)
	deposits, ok := manager.AllStringOutpointsActiveDeposits(
		[]string{h.live.String()}, fsm.EmptyState,
	)
	require.True(t, ok)
	require.Len(t, deposits, 1)
	h.live = deposits[0]
	h.manager.cfg.DepositManager = manager
	return manager
}

// TestAmbiguousPartialClaimsRetainHtlcInputs restores each pending claimant's
// complete original HTLC metadata without granting any of them release rights
// over the ambiguous input shared by the two partially mapped swaps.
func TestAmbiguousPartialClaimsRetainHtlcInputs(t *testing.T) {
	h := newOwnershipRecoveryTest(t)
	a := h.addSwap(t, MonitorInvoiceAndHtlcTx)
	siblings := make([]*deposit.Deposit, 0, 2)
	for i := range 2 {
		d := &deposit.Deposit{
			ID: deposit.ID{byte(i + 2)}, Hash: chainhash.Hash{byte(i + 2)},
			Value: 150_000, TimeOutSweepPkScript: []byte{0x51},
			AddressParams: h.live.AddressParams,
		}
		d.SetState(deposit.LoopingIn)
		require.NoError(t, h.depositStore.CreateDeposit(t.Context(), d))
		siblings = append(siblings, d)
	}
	b := h.addSwap(t, MonitorInvoiceAndHtlcTx, h.live, siblings[0])
	c := h.addSwap(t, MonitorInvoiceAndHtlcTx, siblings[1], h.live)
	expected := make(map[lntypes.Hash]chainhash.Hash)
	for _, swap := range []*StaticAddressLoopIn{b, c} {
		tx, err := swap.createHtlcTx(
			&chaincfg.RegressionNetParams, chainfee.FeePerKwFloor, 1,
		)
		require.NoError(t, err)
		expected[swap.SwapHash] = tx.TxHash()
	}
	h.migrate(t, a.SwapHash)
	manager := h.useRealDepositManager(t)
	pending := h.pending(t)
	require.Len(t, pending, 3)
	for _, swap := range pending {
		if swap.SwapHash != a.SwapHash {
			require.Len(t, swap.Deposits, 1)
		}
	}
	require.NoError(t, h.manager.reconcileLoopInDeposits(t.Context(), pending))
	_, owned := manager.GetLoopInClaim(h.live.OutPoint)
	require.False(t, owned)
	for _, swap := range pending {
		if txHash, ok := expected[swap.SwapHash]; ok {
			require.Len(t, swap.Deposits, 2)
			active, allActive := h.manager.activeDepositsForLoopIn(swap)
			require.True(t, allActive)
			swap.Deposits = active
			tx, err := swap.createHtlcTx(
				&chaincfg.RegressionNetParams, chainfee.FeePerKwFloor, 1,
			)
			require.NoError(t, err)
			require.Equal(t, txHash, tx.TxHash())
		}
		require.NoError(t, manager.ReleaseLoopInDeposits(
			t.Context(), swap.Deposits, swap.SwapHash,
		))
	}
	require.Equal(t, deposit.LoopingIn, h.live.GetState())
	stored, err := h.depositStore.GetDeposit(t.Context(), h.live.ID)
	require.NoError(t, err)
	require.Equal(t, deposit.LoopingIn, stored.GetState())
}
