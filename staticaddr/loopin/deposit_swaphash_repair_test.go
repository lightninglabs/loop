package loopin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

var (
	// repairTestKnownCost is the on-chain cost of every deposit in the
	// repair tests, and so of a swap that spent one of them.
	repairTestKnownCost = OnchainCost{
		Amount: 1_550, Status: OnchainCostComplete,
	}

	// repairTestUnknownCost is the on-chain cost of a swap that has no
	// deposits.
	repairTestUnknownCost = OnchainCost{Status: OnchainCostUnknown}
)

// repairTestContext holds the stores of a deposit swap hash repair test.
type repairTestContext struct {
	t            *testing.T
	baseDB       *loopdb.BaseDB
	ctx          context.Context
	db           loopdb.SwapStore
	depositStore *deposit.SqlStore
	swapStore    *SqlStore
	testClock    *clock.TestClock
	clientPubKey *btcec.PublicKey
	serverPubKey *btcec.PublicKey
	addr         btcutil.Address
	wallet       *repairTestWallet
}

// repairTestWallet is a wallet that knows the given transactions and records
// the start heights it was asked for. If err is set, it fails to list them.
type repairTestWallet struct {
	txs          []lndclient.Transaction
	startHeights []int32
	err          error
}

// ListTransactions returns the transactions of the wallet.
func (w *repairTestWallet) ListTransactions(_ context.Context, startHeight,
	_ int32, _ ...lndclient.ListTransactionsOption) ([]lndclient.Transaction,
	error) {

	w.startHeights = append(w.startHeights, startHeight)

	return w.txs, w.err
}

// newRepairTestContext returns a repair test context with empty stores.
func newRepairTestContext(t *testing.T) *repairTestContext {
	testDb := loopdb.NewTestDB(t)
	t.Cleanup(func() { testDb.Close() })

	testClock := clock.NewTestClock(time.Now())

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	return &repairTestContext{
		t:            t,
		baseDB:       testDb.BaseDB,
		ctx:          t.Context(),
		db:           loopdb.NewStoreMock(t),
		depositStore: deposit.NewSqlStore(testDb.BaseDB),
		swapStore: NewSqlStore(
			loopdb.NewTypedStore[Querier](testDb), testClock,
			&chaincfg.RegressionNetParams,
		),
		testClock:    testClock,
		clientPubKey: clientPubKey,
		serverPubKey: serverPubKey,
		addr:         addr,
		wallet:       &repairTestWallet{},
	}
}

// newDeposit stores a deposit in the given state with a known on-chain cost.
func (c *repairTestContext) newDeposit(hash byte,
	state fsm.StateType) *deposit.Deposit {

	id, err := deposit.GetRandomDepositID()
	require.NoError(c.t, err)

	d := &deposit.Deposit{
		ID:                   id,
		Hash:                 chainhash.Hash{hash},
		Index:                0,
		Value:                btcutil.Amount(100_000),
		ConfirmationHeight:   int64(hash) * 10,
		TimeOutSweepPkScript: []byte{0x00, 0x14, hash},
		OnchainCost: deposit.OnchainCost{
			Amount: repairTestKnownCost.Amount,
			Status: deposit.OnchainCostKnown,
		},
	}
	d.SetState(state)
	require.NoError(c.t, c.depositStore.CreateDeposit(c.ctx, d))

	return d
}

// newSwap stores a swap in the given state that selected the deposits and
// maps them to it, so that the swap stored last owns a deposit. If sweepTx is
// set, the swap published an htlc timeout sweep.
func (c *repairTestContext) newSwap(hash byte, state fsm.StateType,
	sweepTx bool, deposits ...*deposit.Deposit) lntypes.Hash {

	outpoints := make([]string, 0, len(deposits))
	for _, d := range deposits {
		outpoints = append(outpoints, d.OutPoint.String())
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:                lntypes.Hash{hash},
		SwapPreimage:            lntypes.Preimage{hash},
		ClientPubkey:            c.clientPubKey,
		ServerPubkey:            c.serverPubKey,
		HtlcTimeoutSweepAddress: c.addr,
		DepositOutpoints:        outpoints,
		Deposits:                deposits,
	}
	loopIn.SetState(state)
	require.NoError(c.t, c.swapStore.CreateLoopIn(c.ctx, loopIn))

	if sweepTx {
		loopIn.HtlcTimeoutSweepTxHash = &chainhash.Hash{hash}

		// The latest update is the one with the latest time.
		c.testClock.SetTime(c.testClock.Now().Add(time.Second))
		require.NoError(c.t, c.swapStore.UpdateLoopIn(c.ctx, loopIn))
	}

	return loopIn.SwapHash
}

// unmap clears the swap hash of the deposit.
func (c *repairTestContext) unmap(d *deposit.Deposit) {
	_, err := c.baseDB.ExecContext(
		c.ctx, "UPDATE deposits SET swap_hash = NULL "+
			"WHERE deposit_id = $1", d.ID[:],
	)
	require.NoError(c.t, err)
}

// spend adds a confirmed wallet tx that spends exactly the given deposits and
// pays to the htlc of the given swap, like that swap's htlc tx.
func (c *repairTestContext) spend(swapHash lntypes.Hash,
	deposits ...*deposit.Deposit) {

	swap, err := c.swapStore.GetLoopInByHash(c.ctx, swapHash)
	require.NoError(c.t, err)
	htlc, err := swap.getHtlc(&chaincfg.RegressionNetParams)
	require.NoError(c.t, err)

	tx := wire.NewMsgTx(2)
	for _, d := range deposits {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: d.OutPoint})
	}
	tx.AddTxOut(&wire.TxOut{Value: 100_000, PkScript: htlc.PkScript})

	c.wallet.txs = append(c.wallet.txs, lndclient.Transaction{
		Tx:            tx,
		Confirmations: 1,
	})
}

// conflict adds an unconfirmed wallet tx that spends the given deposits.
func (c *repairTestContext) conflict(deposits ...*deposit.Deposit) {
	tx := wire.NewMsgTx(2)
	for _, d := range deposits {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: d.OutPoint})
	}

	c.wallet.txs = append(c.wallet.txs, lndclient.Transaction{Tx: tx})
}

// owner returns the swap the deposit is mapped to.
func (c *repairTestContext) owner(d *deposit.Deposit) lntypes.Hash {
	swapHashes, err := c.swapStore.SwapHashesForDepositIDs(
		c.ctx, []deposit.ID{d.ID},
	)
	require.NoError(c.t, err)
	require.Len(c.t, swapHashes, 1)

	for swapHash := range swapHashes {
		return swapHash
	}

	return lntypes.Hash{}
}

// remap maps the deposit to the given swap.
func (c *repairTestContext) remap(d *deposit.Deposit, swapHash lntypes.Hash) {
	require.NoError(c.t, c.swapStore.BatchMapDepositsToSwapHashes(
		c.ctx, map[deposit.ID]lntypes.Hash{d.ID: swapHash},
	))
}

// loadedDeposits returns the deposits a swap loads after a restart.
func (c *repairTestContext) loadedDeposits(
	swapHash lntypes.Hash) []wire.OutPoint {

	swap, err := c.swapStore.GetLoopInByHash(c.ctx, swapHash)
	require.NoError(c.t, err)

	outpoints := make([]wire.OutPoint, 0, len(swap.Deposits))
	for _, d := range swap.Deposits {
		outpoints = append(outpoints, d.OutPoint)
	}

	return outpoints
}

// onchainCost returns the on-chain cost that loopd reports for a swap.
func (c *repairTestContext) onchainCost(swapHash lntypes.Hash) OnchainCost {
	swap, err := c.swapStore.GetLoopInByHash(c.ctx, swapHash)
	require.NoError(c.t, err)

	deposits, err := c.swapStore.DepositsForSwap(c.ctx, swapHash)
	require.NoError(c.t, err)

	return SwapOnchainCost(swap, deposits)
}

// migrationDone returns whether the migration with the given id ran.
func (c *repairTestContext) migrationDone(migrationID string) bool {
	done, err := c.db.HasMigration(c.ctx, migrationID)
	require.NoError(c.t, err)

	return done
}

// TestRepairHtlcTimeoutDepositSwapHash tests that the repair maps each deposit
// on the htlc timeout path to the swap whose htlc tx spent it, and leaves all
// other deposits as they are.
func TestRepairHtlcTimeoutDepositSwapHash(t *testing.T) {
	c := newRepairTestContext(t)

	repair := func() {
		require.NoError(t, MigrateRepairHtlcTimeoutDepositSwapHash(
			c.ctx, c.db, c.depositStore, c.swapStore, c.wallet,
		))
	}

	// A swap gave the deposit back after its payment deadline and a later
	// swap selected it, but the first swap's htlc confirmed after all, so
	// it swept the htlc timeout path. The wallet also lists an unconfirmed
	// tx that spends the deposit, after the confirmed one.
	reclaimed := c.newDeposit(2, deposit.HtlcTimeoutSwept)
	swept := c.newSwap(0xc, HtlcTimeoutSwept, false, reclaimed)
	c.newSwap(0xd, Failed, false, reclaimed)
	c.spend(swept, reclaimed)
	c.conflict(reclaimed)

	// The same, but the first swap failed after it published the htlc
	// timeout sweep.
	reclaimedFailed := c.newDeposit(3, deposit.SweepHtlcTimeout)
	failedSweeping := c.newSwap(0xe, Failed, true, reclaimedFailed)
	c.newSwap(0xf, Failed, false, reclaimedFailed)
	c.spend(failedSweeping, reclaimedFailed)

	// The same, but the first swap still waits for its htlc timeout sweep
	// to confirm, so it loads its deposits again after a restart. It also
	// selected a deposit that the later swap didn't take.
	reclaimedPending := c.newDeposit(7, deposit.SweepHtlcTimeout)
	kept := c.newDeposit(8, deposit.SweepHtlcTimeout)
	pending := c.newSwap(
		0x17, MonitorHtlcTimeoutSweep, false, reclaimedPending, kept,
	)
	c.newSwap(0x18, Failed, false, reclaimedPending)
	c.spend(pending, reclaimedPending, kept)

	// A tx paying to the htlc script of a swap moved it to its htlc timeout
	// path, but another swap's htlc tx spent the deposit. That swap still
	// waits for its htlc timeout, so it doesn't qualify either.
	decoyed := c.newDeposit(9, deposit.SweepHtlcTimeout)
	c.newSwap(0x19, HtlcTimeoutSwept, false, decoyed)
	realSpender := c.newSwap(
		0x1a, MonitorInvoiceAndHtlcTx, false, decoyed,
	)
	c.spend(
		realSpender, decoyed, c.newDeposit(10, deposit.SweepHtlcTimeout),
	)

	// The wallet doesn't know the tx that spent the deposit.
	unknownTx := c.newDeposit(11, deposit.HtlcTimeoutSwept)
	c.newSwap(0x1b, HtlcTimeoutSwept, false, unknownTx)
	unknownTxOwner := c.newSwap(0x1c, Failed, false, unknownTx)

	// No swap that selected the deposit is on its own htlc timeout path.
	noCandidate := c.newDeposit(12, deposit.HtlcTimeoutSwept)
	noCandidateSpender := c.newSwap(0x1d, Failed, false, noCandidate)
	noCandidateOwner := c.newSwap(0x1e, Failed, false, noCandidate)
	c.spend(noCandidateSpender, noCandidate)

	// The deposit isn't mapped to any swap.
	unmapped := c.newDeposit(13, deposit.HtlcTimeoutSwept)
	unmappedSpender := c.newSwap(0x1f, HtlcTimeoutSwept, false, unmapped)
	c.unmap(unmapped)
	c.spend(unmappedSpender, unmapped)

	// Two swaps on their htlc timeout path selected exactly the same
	// deposit, one of them through a tx paying to its htlc script. The
	// tx that spent it pays to the htlc of the other one.
	sameDeposit := c.newDeposit(4, deposit.HtlcTimeoutSwept)
	c.newSwap(0x10, HtlcTimeoutSwept, false, sameDeposit)
	sameDepositSpender := c.newSwap(
		0x11, SweepHtlcTimeout, false, sameDeposit,
	)
	c.newSwap(0x16, Failed, false, sameDeposit)
	c.spend(sameDepositSpender, sameDeposit)

	// The deposit isn't on the htlc timeout path, so this repair leaves it,
	// even if a swap on its htlc timeout path selected it.
	looped := c.newDeposit(5, deposit.LoopedIn)
	loopedSpender := c.newSwap(0x12, HtlcTimeoutSwept, false, looped)
	loopedOwner := c.newSwap(0x13, Failed, false, looped)
	c.spend(loopedSpender, looped)

	// The deposit is already mapped to the swap that spent it.
	correct := c.newDeposit(6, deposit.HtlcTimeoutSwept)
	c.newSwap(0x14, Failed, false, correct)
	correctOwner := c.newSwap(0x15, HtlcTimeoutSwept, false, correct)
	c.spend(correctOwner, correct)

	// Before the repair, the pending swap loads only kept, and the swaps
	// that spent the deposits report no cost.
	require.Equal(
		t, []wire.OutPoint{kept.OutPoint}, c.loadedDeposits(pending),
	)
	require.Equal(t, repairTestUnknownCost, c.onchainCost(swept))
	require.Equal(t, repairTestUnknownCost, c.onchainCost(failedSweeping))

	repair()

	require.Equal(t, swept, c.owner(reclaimed))
	require.Equal(t, failedSweeping, c.owner(reclaimedFailed))
	require.Equal(t, pending, c.owner(reclaimedPending))
	require.Equal(t, realSpender, c.owner(decoyed))
	require.Equal(t, unknownTxOwner, c.owner(unknownTx))
	require.Equal(t, noCandidateOwner, c.owner(noCandidate))
	require.Equal(t, unmappedSpender, c.owner(unmapped))
	require.Equal(t, sameDepositSpender, c.owner(sameDeposit))
	require.Equal(t, loopedOwner, c.owner(looped))
	require.Equal(t, correctOwner, c.owner(correct))

	require.ElementsMatch(
		t, []wire.OutPoint{reclaimedPending.OutPoint, kept.OutPoint},
		c.loadedDeposits(pending),
	)
	require.Equal(t, repairTestKnownCost, c.onchainCost(swept))
	require.Equal(t, repairTestKnownCost, c.onchainCost(failedSweeping))

	// The wallet txs are only needed from the block in which the first
	// deposit on the htlc timeout path confirmed.
	require.Equal(t, []int32{20}, c.wallet.startHeights)

	// The repair runs only once.
	require.True(t, c.migrationDone(htlcTimeoutRepairMigrationID))

	c.remap(reclaimed, correctOwner)
	repair()
	require.Equal(t, correctOwner, c.owner(reclaimed))
}

// TestRepairHtlcTimeoutDepositSwapHashWithoutDeposits tests that the repair
// doesn't list the wallet txs if no deposit is on the htlc timeout path.
func TestRepairHtlcTimeoutDepositSwapHashWithoutDeposits(t *testing.T) {
	c := newRepairTestContext(t)

	looped := c.newDeposit(1, deposit.LoopedIn)
	c.newSwap(0xa, Succeeded, false, looped)

	require.NoError(t, MigrateRepairHtlcTimeoutDepositSwapHash(
		c.ctx, c.db, c.depositStore, c.swapStore, c.wallet,
	))

	require.Empty(t, c.wallet.startHeights)
	require.True(t, c.migrationDone(htlcTimeoutRepairMigrationID))
}

// TestRepairHtlcTimeoutDepositSwapHashWalletError tests that the repair
// doesn't fail if lnd can't list the wallet txs, and runs again at the next
// start.
func TestRepairHtlcTimeoutDepositSwapHashWalletError(t *testing.T) {
	c := newRepairTestContext(t)

	reclaimed := c.newDeposit(2, deposit.HtlcTimeoutSwept)
	swept := c.newSwap(0xc, HtlcTimeoutSwept, false, reclaimed)
	owner := c.newSwap(0xd, Failed, false, reclaimed)
	c.spend(swept, reclaimed)

	repair := func() {
		require.NoError(t, MigrateRepairHtlcTimeoutDepositSwapHash(
			c.ctx, c.db, c.depositStore, c.swapStore, c.wallet,
		))
	}

	c.wallet.err = errors.New("timeout")
	repair()
	require.Equal(t, owner, c.owner(reclaimed))
	require.False(t, c.migrationDone(htlcTimeoutRepairMigrationID))

	c.wallet.err = nil
	repair()
	require.Equal(t, swept, c.owner(reclaimed))
	require.True(t, c.migrationDone(htlcTimeoutRepairMigrationID))
}
