package loopin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

var (
	// testStaticPkScript is the pkScript of the static address outputs in
	// the test funding transactions.
	testStaticPkScript = []byte{0x51, 0x20, 0x01}

	// testStaticPkScript2 is the pkScript of a second static address.
	testStaticPkScript2 = []byte{0x51, 0x20, 0x03}

	// testOtherPkScript is the pkScript of change and other outputs in the
	// test funding transactions.
	testOtherPkScript = []byte{0x00, 0x14, 0x02}
)

// staticOut returns a static address output with the given value.
func staticOut(value int64) *wire.TxOut {
	return wire.NewTxOut(value, testStaticPkScript)
}

// staticOut2 returns an output to the second static address.
func staticOut2(value int64) *wire.TxOut {
	return wire.NewTxOut(value, testStaticPkScript2)
}

// otherOut returns a change output with the given value.
func otherOut(value int64) *wire.TxOut {
	return wire.NewTxOut(value, testOtherPkScript)
}

// fundingTx returns a transaction with the given outputs. The seed makes the
// transaction hash unique.
func fundingTx(seed uint32, outputs ...*wire.TxOut) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: seed},
	})
	for _, out := range outputs {
		tx.AddTxOut(out)
	}

	return tx
}

// walletTx returns the wallet view of a funding transaction. If walletFunded is
// false, the transaction input doesn't belong to the wallet.
func walletTx(tx *wire.MsgTx, fee btcutil.Amount,
	walletFunded bool) lndclient.Transaction {

	return lndclient.Transaction{
		Tx:     tx,
		TxHash: tx.TxHash().String(),
		Fee:    fee,
		PreviousOutpoints: []*lnrpc.PreviousOutPoint{{
			Outpoint:    tx.TxIn[0].PreviousOutPoint.String(),
			IsOurOutput: walletFunded,
		}},
	}
}

// depositOf returns the deposit for the given output of a funding transaction.
func depositOf(tx *wire.MsgTx, index uint32) *deposit.Deposit {
	return &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: index,
		},
		Value:              btcutil.Amount(tx.TxOut[index].Value),
		ConfirmationHeight: 100,
	}
}

// knownDeposits returns a deposit lookup that knows the outputs of the given
// transactions that pay to a static address as deposits.
func knownDeposits(txs ...*wire.MsgTx) *mockDepositManager {
	lookup := &mockDepositManager{
		byOutpoint: make(map[string]*deposit.Deposit),
	}
	for _, tx := range txs {
		for i, txOut := range tx.TxOut {
			if bytes.Equal(txOut.PkScript, testOtherPkScript) {
				continue
			}

			d := depositOf(tx, uint32(i))
			lookup.byOutpoint[d.OutPoint.String()] = d
		}
	}

	return lookup
}

// indexes returns a set of output indexes.
func indexes(idx ...uint32) map[uint32]struct{} {
	set := make(map[uint32]struct{}, len(idx))
	for _, i := range idx {
		set[i] = struct{}{}
	}

	return set
}

// txsByHash indexes the given wallet transactions by hash.
func txsByHash(
	txs ...lndclient.Transaction) map[chainhash.Hash]lndclient.Transaction {

	byHash := make(map[chainhash.Hash]lndclient.Transaction, len(txs))
	for _, tx := range txs {
		byHash[tx.Tx.TxHash()] = tx
	}

	return byHash
}

// TestDepositFeeShare tests that funding transaction fees are split across the
// static address outputs by value.
func TestDepositFeeShare(t *testing.T) {
	tests := []struct {
		name     string
		tx       *wire.MsgTx
		fee      btcutil.Amount
		deposits map[uint32]struct{}
		shares   map[uint32]btcutil.Amount
	}{
		{
			name: "single deposit with change",
			tx: fundingTx(
				0, otherOut(700_000), staticOut(300_000),
			),
			fee:      1_550,
			deposits: indexes(1),
			shares:   map[uint32]btcutil.Amount{1: 1_550},
		},
		{
			name: "split by value, change not charged",
			tx: fundingTx(
				0, staticOut(300_000), staticOut(100_000),
				otherOut(500_000),
			),
			fee:      1_500,
			deposits: indexes(0, 1),
			shares:   map[uint32]btcutil.Amount{0: 1_125, 1: 375},
		},
		{
			name: "remainder to lowest index",
			tx: fundingTx(
				0, staticOut(100_000), staticOut(100_000),
				staticOut(100_000),
			),
			fee:      1_000,
			deposits: indexes(0, 1, 2),
			shares: map[uint32]btcutil.Amount{
				0: 334, 1: 333, 2: 333,
			},
		},
		{
			name: "remainder to lowest static index",
			tx: fundingTx(
				0, otherOut(50_000), staticOut(100_000),
				staticOut(100_000), staticOut(100_000),
			),
			fee:      1_000,
			deposits: indexes(1, 2, 3),
			shares: map[uint32]btcutil.Amount{
				1: 334, 2: 333, 3: 333,
			},
		},
		{
			name:     "zero fee",
			tx:       fundingTx(0, staticOut(100_000)),
			fee:      0,
			deposits: indexes(0),
			shares:   map[uint32]btcutil.Amount{0: 0},
		},
		{
			name: "large amounts don't overflow",
			tx: fundingTx(
				0, staticOut(2_000_000_000_000_000),
				staticOut(100_000_000_000_000),
			),
			fee:      100_000_000,
			deposits: indexes(0, 1),
			shares: map[uint32]btcutil.Amount{
				0: 95_238_096, 1: 4_761_904,
			},
		},
		{
			name: "deposits to different static addresses",
			tx: fundingTx(
				0, staticOut(100_000), staticOut2(100_000),
				otherOut(50_000),
			),
			fee:      1_000,
			deposits: indexes(0, 1),
			shares:   map[uint32]btcutil.Amount{0: 500, 1: 500},
		},
		{
			name: "deposit not returned by lookup is charged",
			tx: fundingTx(
				0, staticOut(100_000), otherOut(50_000),
			),
			fee:      1_000,
			deposits: indexes(),
			shares:   map[uint32]btcutil.Amount{0: 1_000},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var total btcutil.Amount
			for index, expected := range tc.shares {
				share, err := depositFeeShare(
					tc.tx, tc.fee, index, tc.deposits,
				)
				require.NoError(t, err)
				require.Equal(t, expected, share)

				total += share
			}

			// The shares of all deposits add up to the full fee.
			require.Equal(t, tc.fee, total)
		})
	}
}

// TestDepositFeeShareErrors tests invalid fee share inputs.
func TestDepositFeeShareErrors(t *testing.T) {
	tx := fundingTx(0, staticOut(100_000))

	_, err := depositFeeShare(tx, 1_000, 1, indexes(0))
	require.ErrorContains(t, err, "out of range")

	_, err = depositFeeShare(tx, -1, 0, indexes(0))
	require.ErrorContains(t, err, "negative fee")

	_, err = depositFeeShare(
		fundingTx(0, staticOut(0)), 1_000, 0, indexes(0),
	)
	require.ErrorContains(t, err, "no value")
}

// TestOnchainCostFromTxs tests the on-chain cost of the deposits of a swap.
func TestOnchainCostFromTxs(t *testing.T) {
	txA := fundingTx(1, otherOut(700_000), staticOut(300_000))
	txB := fundingTx(2, staticOut(200_000), otherOut(10_000))
	txC := fundingTx(3, staticOut(300_000), staticOut(100_000))
	txExternal := fundingTx(4, staticOut(500_000))

	txs := txsByHash(
		walletTx(txA, 1_550, true), walletTx(txB, 900, true),
		walletTx(txC, 1_500, true), walletTx(txExternal, 2_000, false),
	)
	lookup := knownDeposits(txA, txB, txC, txExternal)

	tests := []struct {
		name     string
		deposits []*deposit.Deposit
		cost     btcutil.Amount
		known    bool
	}{
		{
			name:     "single deposit",
			deposits: []*deposit.Deposit{depositOf(txA, 1)},
			cost:     1_550,
			known:    true,
		},
		{
			name: "deposits from different transactions",
			deposits: []*deposit.Deposit{
				depositOf(txA, 1), depositOf(txB, 0),
			},
			cost:  1_550 + 900,
			known: true,
		},
		{
			name: "deposits from the same transaction",
			deposits: []*deposit.Deposit{
				depositOf(txC, 0), depositOf(txC, 1),
			},
			cost:  1_500,
			known: true,
		},
		{
			name: "one deposit of a shared transaction",
			deposits: []*deposit.Deposit{
				depositOf(txC, 1),
			},
			cost:  375,
			known: true,
		},
		{
			name:     "externally funded deposit",
			deposits: []*deposit.Deposit{depositOf(txExternal, 0)},
			known:    false,
		},
		{
			name: "known and externally funded deposits",
			deposits: []*deposit.Deposit{
				depositOf(txA, 1), depositOf(txExternal, 0),
			},
			known: false,
		},
		{
			name: "funding transaction not in wallet",
			deposits: []*deposit.Deposit{
				depositOf(fundingTx(5, staticOut(100_000)), 0),
			},
			known: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cost, known, err := onchainCostFromTxs(
				t.Context(), lookup, txs, tc.deposits,
			)
			require.NoError(t, err)
			require.Equal(t, tc.known, known)
			require.Equal(t, tc.cost, cost)
		})
	}

	_, _, err := onchainCostFromTxs(t.Context(), lookup, txs, nil)
	require.Error(t, err)
}

// TestOnchainCostAcrossSwaps tests that deposits of one funding transaction
// that are used in different swaps don't count the fee more than once, also
// when the deposits went to different static addresses.
func TestOnchainCostAcrossSwaps(t *testing.T) {
	tests := []struct {
		name string
		tx   *wire.MsgTx
	}{
		{
			name: "same static address",
			tx: fundingTx(
				0, staticOut(250_000), otherOut(400_000),
				staticOut(150_000),
			),
		},
		{
			name: "different static addresses",
			tx: fundingTx(
				0, staticOut(250_000), otherOut(400_000),
				staticOut2(150_000),
			),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			txs := txsByHash(walletTx(tc.tx, 1_001, true))
			lookup := knownDeposits(tc.tx)

			swap1, known, err := onchainCostFromTxs(
				t.Context(), lookup, txs,
				[]*deposit.Deposit{depositOf(tc.tx, 2)},
			)
			require.NoError(t, err)
			require.True(t, known)

			swap2, known, err := onchainCostFromTxs(
				t.Context(), lookup, txs,
				[]*deposit.Deposit{depositOf(tc.tx, 0)},
			)
			require.NoError(t, err)
			require.True(t, known)

			require.Equal(t, btcutil.Amount(375), swap1)
			require.Equal(t, btcutil.Amount(626), swap2)
			require.Equal(t, btcutil.Amount(1_001), swap1+swap2)
		})
	}
}

// TestOnchainCostSiblingRecordedLater tests that a deposit's share doesn't
// depend on whether a sibling deposit to the same address is already stored,
// so a fee is never counted more than once.
func TestOnchainCostSiblingRecordedLater(t *testing.T) {
	tx := fundingTx(
		0, staticOut(100_000), otherOut(50_000), staticOut(100_000),
	)
	txs := txsByHash(walletTx(tx, 1_001, true))
	first := []*deposit.Deposit{depositOf(tx, 0)}
	second := []*deposit.Deposit{depositOf(tx, 2)}

	// Only the first deposit is stored when it is swapped.
	lookup := &mockDepositManager{
		byOutpoint: map[string]*deposit.Deposit{
			first[0].OutPoint.String(): first[0],
		},
	}
	before, known, err := onchainCostFromTxs(
		t.Context(), lookup, txs, first,
	)
	require.NoError(t, err)
	require.True(t, known)

	// Later, the sibling is stored and swapped too.
	lookup = knownDeposits(tx)
	after, _, err := onchainCostFromTxs(t.Context(), lookup, txs, first)
	require.NoError(t, err)
	sibling, _, err := onchainCostFromTxs(t.Context(), lookup, txs, second)
	require.NoError(t, err)

	require.Equal(t, btcutil.Amount(501), before)
	require.Equal(t, before, after)
	require.Equal(t, btcutil.Amount(500), sibling)
	require.Equal(t, btcutil.Amount(1_001), before+sibling)
}

// TestOnchainCostChangeDeposit tests that a deposit created as change by a
// transaction that spent earlier deposits, like a partial loop-in or a partial
// withdrawal, costs nothing, since it brought in no new funds.
func TestOnchainCostChangeDeposit(t *testing.T) {
	funding := fundingTx(1, otherOut(10_000), staticOut(1_000_000))
	fundingDeposit := depositOf(funding, 1)

	// spendTx spends the funding deposit, and any extra inputs, and sends
	// change back to the static address at index 1.
	spendTx := func(extra ...wire.OutPoint) *wire.MsgTx {
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: fundingDeposit.OutPoint,
		})
		for _, op := range extra {
			tx.AddTxIn(&wire.TxIn{PreviousOutPoint: op})
		}
		tx.AddTxOut(otherOut(898_000))
		tx.AddTxOut(staticOut(100_000))

		return tx
	}

	foreign := wire.OutPoint{Hash: chainhash.Hash{0xff}, Index: 3}
	tests := []struct {
		name  string
		spend *wire.MsgTx
		ours  []bool
	}{
		{
			name:  "change of our own spend",
			spend: spendTx(),
			ours:  []bool{true},
		},
		{
			name:  "change of a batched spend",
			spend: spendTx(foreign),
			ours:  []bool{true, false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spend := walletTx(tc.spend, 2_000, true)
			spend.PreviousOutpoints = nil
			for i, txIn := range tc.spend.TxIn {
				spend.PreviousOutpoints = append(
					spend.PreviousOutpoints,
					&lnrpc.PreviousOutPoint{
						Outpoint:    txIn.PreviousOutPoint.String(),
						IsOurOutput: tc.ours[i],
					},
				)
			}

			fresh := fundingTx(2, staticOut(200_000), otherOut(5_000))
			txs := txsByHash(
				walletTx(funding, 1_500, true), spend,
				walletTx(fresh, 700, true),
			)
			lookup := knownDeposits(funding, tc.spend, fresh)
			change := depositOf(tc.spend, 1)

			cost, known, err := onchainCostFromTxs(
				t.Context(), lookup, txs, []*deposit.Deposit{change},
			)
			require.NoError(t, err)
			require.True(t, known)
			require.Zero(t, cost)

			// Together with a newly funded deposit, only the new
			// deposit's fee is counted.
			cost, known, err = onchainCostFromTxs(
				t.Context(), lookup, txs, []*deposit.Deposit{
					change, depositOf(fresh, 0),
				},
			)
			require.NoError(t, err)
			require.True(t, known)
			require.Equal(t, btcutil.Amount(700), cost)
		})
	}
}

// errLookup is a deposit lookup that always fails.
type errLookup struct{}

// DepositsForOutpoints returns an error.
func (errLookup) DepositsForOutpoints(context.Context, []string,
	bool) ([]*deposit.Deposit, error) {

	return nil, errors.New("deposit store unavailable")
}

// TestOnchainCostLookupError tests that deposit lookup errors are returned.
func TestOnchainCostLookupError(t *testing.T) {
	tx := fundingTx(0, staticOut(100_000))

	_, _, err := onchainCostFromTxs(
		t.Context(), errLookup{}, txsByHash(walletTx(tx, 500, true)),
		[]*deposit.Deposit{depositOf(tx, 0)},
	)
	require.ErrorContains(t, err, "deposit store unavailable")
}

// fakeTxGetter is a wallet transaction getter for tests. It returns the given
// transactions, lnd's not found error for others, or err if set, and counts
// the lookups.
type fakeTxGetter struct {
	txs   map[chainhash.Hash]lndclient.Transaction
	err   error
	calls int
}

// GetTransaction returns the transaction with the given hash.
func (f *fakeTxGetter) GetTransaction(_ context.Context,
	txid chainhash.Hash) (lndclient.Transaction, error) {

	f.calls++
	if f.err != nil {
		return lndclient.Transaction{}, f.err
	}

	tx, ok := f.txs[txid]
	if !ok {
		return lndclient.Transaction{}, fmt.Errorf("can not find "+
			"transaction: txid %v", txid)
	}

	return tx, nil
}

// TestFundingTransactions tests the lookup of deposit funding transactions.
func TestFundingTransactions(t *testing.T) {
	tx := fundingTx(0, staticOut(100_000), staticOut(200_000))
	missing := fundingTx(1, staticOut(100_000))
	deposits := []*deposit.Deposit{
		depositOf(tx, 0), depositOf(tx, 1), depositOf(missing, 0),
	}

	getter := &fakeTxGetter{
		txs: txsByHash(walletTx(tx, 500, true)),
	}
	txs, err := fundingTransactions(t.Context(), getter, deposits)
	require.NoError(t, err)

	// Each funding transaction is only looked up once, and transactions
	// that aren't in the wallet are left out.
	require.Equal(t, 2, getter.calls)
	require.Len(t, txs, 1)
	require.Contains(t, txs, tx.TxHash())

	// Other errors are returned.
	getter = &fakeTxGetter{err: errors.New("wallet unavailable")}
	_, err = fundingTransactions(t.Context(), getter, deposits)
	require.ErrorContains(t, err, "wallet unavailable")
}

// TestWithOnchainCost tests that the on-chain cost is set when an action
// reports that the swap consumed its deposits.
func TestWithOnchainCost(t *testing.T) {
	tx := fundingTx(0, otherOut(700_000), staticOut(300_000))
	external := fundingTx(1, staticOut(300_000))

	newFSM := func(deposits ...*deposit.Deposit) *FSM {
		mockLnd := test.NewMockLnd()
		mockLnd.Transactions = []lndclient.Transaction{
			walletTx(tx, 1_550, true),
			walletTx(external, 2_000, false),
		}

		return &FSM{
			StateMachine: &fsm.StateMachine{},
			cfg: &Config{
				WalletKit:      mockLnd.WalletKit,
				DepositManager: knownDeposits(tx, external),
			},
			loopIn: &StaticAddressLoopIn{
				DepositOutpoints: outpointsOf(deposits...),
			},
		}
	}

	returning := func(event fsm.EventType) fsm.Action {
		return func(context.Context, fsm.EventContext) fsm.EventType {
			return event
		}
	}

	t.Run("consumed", func(t *testing.T) {
		f := newFSM(depositOf(tx, 1))
		action := f.withOnchainCost(
			returning(OnPaymentReceived), OnPaymentReceived,
		)

		require.Equal(t, OnPaymentReceived, action(t.Context(), nil))
		require.NotNil(t, f.loopIn.OnchainCost)
		require.Equal(t, btcutil.Amount(1_550), *f.loopIn.OnchainCost)
	})

	t.Run("not consumed", func(t *testing.T) {
		f := newFSM(depositOf(tx, 1))
		action := f.withOnchainCost(
			returning(OnRecover), OnPaymentReceived,
		)

		require.Equal(t, OnRecover, action(t.Context(), nil))
		require.Nil(t, f.loopIn.OnchainCost)
	})

	t.Run("unknown fee", func(t *testing.T) {
		f := newFSM(depositOf(external, 0))
		action := f.withOnchainCost(
			returning(OnHtlcTimeoutSwept), OnHtlcTimeoutSwept,
		)

		require.Equal(t, OnHtlcTimeoutSwept, action(t.Context(), nil))
		require.Nil(t, f.loopIn.OnchainCost)
	})

	t.Run("existing cost kept", func(t *testing.T) {
		f := newFSM(depositOf(tx, 1))
		existing := btcutil.Amount(42)
		f.loopIn.OnchainCost = &existing

		action := f.withOnchainCost(
			returning(OnPaymentReceived), OnPaymentReceived,
		)

		require.Equal(t, OnPaymentReceived, action(t.Context(), nil))
		require.Equal(t, existing, *f.loopIn.OnchainCost)
	})
}

// flakyWalletKit fails a given number of transaction lookups before it uses
// the wrapped wallet kit, and counts the lookups.
type flakyWalletKit struct {
	lndclient.WalletKitClient

	failures int
	calls    int
}

// GetTransaction fails while failures are left.
func (f *flakyWalletKit) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (lndclient.Transaction, error) {

	f.calls++
	if f.failures > 0 {
		f.failures--

		return lndclient.Transaction{}, errors.New("lnd unavailable")
	}

	return f.WalletKitClient.GetTransaction(ctx, txid)
}

// TestSetOnchainCostLookupError tests that a lookup error leaves the cost
// unknown after a single attempt, so that the swap isn't held up.
func TestSetOnchainCostLookupError(t *testing.T) {
	tx := fundingTx(0, otherOut(700_000), staticOut(300_000))

	mockLnd := test.NewMockLnd()
	mockLnd.Transactions = []lndclient.Transaction{
		walletTx(tx, 1_550, true),
	}
	walletKit := &flakyWalletKit{
		WalletKitClient: mockLnd.WalletKit,
		failures:        1,
	}

	f := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			WalletKit:      walletKit,
			DepositManager: knownDeposits(tx),
		},
		loopIn: &StaticAddressLoopIn{
			DepositOutpoints: outpointsOf(depositOf(tx, 1)),
		},
	}
	f.setOnchainCost(t.Context())

	require.Nil(t, f.loopIn.OnchainCost)
	require.Equal(t, 1, walletKit.calls)
}

// outpointsOf returns the outpoints of the given deposits as strings.
func outpointsOf(deposits ...*deposit.Deposit) []string {
	outpoints := make([]string, len(deposits))
	for i, d := range deposits {
		outpoints[i] = d.OutPoint.String()
	}

	return outpoints
}

// TestOnchainCostPersistedOnRecovery runs the loop-in FSM from a stored swap
// in MonitorInvoiceAndHtlcTx whose invoice is already paid, as after a
// restart, and checks that the on-chain cost is persisted with the swap.
func TestOnchainCostPersistedOnRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	c := newOnchainCostTestContext(t)
	tx := fundingTx(0, otherOut(700_000), staticOut(300_000))
	swapHash := lntypes.Hash{1, 2, 9}

	// The swap is stored without a cost, as if loopd stopped before the
	// paid invoice was processed.
	stored := c.createLoopIn(
		swapHash, MonitorInvoiceAndHtlcTx,
		wire.OutPoint{Hash: tx.TxHash(), Index: 1},
	)
	require.Nil(t, c.onchainCost(swapHash))

	mockLnd := test.NewMockLnd()
	mockLnd.Transactions = []lndclient.Transaction{
		walletTx(tx, 1_550, true),
	}
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractSettled,
	})

	f, _ := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionRejected,
		mockLnd.LndServices.Invoices,
	)
	f.cfg.Store = c.swapStore
	f.loopIn.DepositOutpoints = stored.DepositOutpoints

	resultChan := make(chan error, 1)
	go func() {
		resultChan <- f.SendEvent(ctx, OnRecover, nil)
	}()
	waitForMonitorSubscriptions(t, ctx, mockLnd)

	select {
	case err := <-resultChan:
		require.NoError(t, err)

	case <-ctx.Done():
		t.Fatalf("fsm did not finish: %v", ctx.Err())
	}

	loopIn, err := c.swapStore.GetLoopInByHash(ctx, swapHash)
	require.NoError(t, err)
	require.Equal(t, Succeeded, loopIn.GetState())
	require.NotNil(t, loopIn.OnchainCost)
	require.Equal(t, btcutil.Amount(1_550), *loopIn.OnchainCost)
}
