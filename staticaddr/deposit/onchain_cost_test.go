package deposit

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
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

// walletTx returns the wallet view of a transaction. ours tells for each input
// whether it belongs to the wallet.
func walletTx(tx *wire.MsgTx, fee btcutil.Amount,
	ours ...bool) lndclient.Transaction {

	walletTx := lndclient.Transaction{
		Tx:     tx,
		TxHash: tx.TxHash().String(),
		Fee:    fee,
	}
	for i, txIn := range tx.TxIn {
		walletTx.PreviousOutpoints = append(
			walletTx.PreviousOutpoints, &lnrpc.PreviousOutPoint{
				Outpoint:    txIn.PreviousOutPoint.String(),
				IsOurOutput: ours[i],
			},
		)
	}

	return walletTx
}

// depositOf returns the deposit for the given output of a funding transaction.
// Its ID is derived from the outpoint.
func depositOf(tx *wire.MsgTx, index uint32) *Deposit {
	outpoint := wire.OutPoint{Hash: tx.TxHash(), Index: index}

	return &Deposit{
		ID:       ID(sha256.Sum256([]byte(outpoint.String()))),
		OutPoint: outpoint,
		Value:    btcutil.Amount(tx.TxOut[index].Value),
	}
}

// knownDeposits returns a deposit check that knows the outputs of the given
// transactions that don't pay to testOtherPkScript as deposits.
func knownDeposits(txs ...*wire.MsgTx) func(wire.OutPoint) bool {
	known := make(map[wire.OutPoint]struct{})
	for _, tx := range txs {
		for i, txOut := range tx.TxOut {
			if string(txOut.PkScript) == string(testOtherPkScript) {
				continue
			}

			known[wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: uint32(i),
			}] = struct{}{}
		}
	}

	return func(outpoint wire.OutPoint) bool {
		_, ok := known[outpoint]

		return ok
	}
}

// fakeTxGetter is a wallet transaction getter for tests. It returns the given
// transactions, lnd's not found error for others, or an error for the
// transactions in errs, and counts the lookups.
type fakeTxGetter struct {
	txs  map[chainhash.Hash]lndclient.Transaction
	errs map[chainhash.Hash]error

	// block makes looking up the transaction wait until the context is
	// done.
	block map[chainhash.Hash]struct{}

	calls int
}

// newFakeTxGetter returns a getter that knows the given transactions.
func newFakeTxGetter(txs ...lndclient.Transaction) *fakeTxGetter {
	getter := &fakeTxGetter{
		txs:   make(map[chainhash.Hash]lndclient.Transaction),
		errs:  make(map[chainhash.Hash]error),
		block: make(map[chainhash.Hash]struct{}),
	}
	for _, tx := range txs {
		getter.txs[tx.Tx.TxHash()] = tx
	}

	return getter
}

// GetTransaction returns the transaction with the given hash.
func (f *fakeTxGetter) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (lndclient.Transaction, error) {

	f.calls++
	if _, ok := f.block[txid]; ok {
		<-ctx.Done()

		return lndclient.Transaction{}, ctx.Err()
	}

	if err, ok := f.errs[txid]; ok {
		return lndclient.Transaction{}, err
	}

	tx, ok := f.txs[txid]
	if !ok {
		return lndclient.Transaction{}, fmt.Errorf("%v: txid %v",
			lndTxNotFound, txid)
	}

	return tx, nil
}

// indexes returns a set of output indexes.
func indexes(idx ...uint32) map[uint32]struct{} {
	set := make(map[uint32]struct{}, len(idx))
	for _, i := range idx {
		set[i] = struct{}{}
	}

	return set
}

// knownCost returns a known on-chain cost of the given amount.
func knownCost(amount btcutil.Amount) OnchainCost {
	return OnchainCost{Amount: amount, Status: OnchainCostKnown}
}

// unknownCost is the on-chain cost of a deposit whose funding fee is unknown.
var unknownCost = OnchainCost{Status: OnchainCostUnknown}

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
			name: "deposit not known yet is charged",
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

// TestOnchainCosts tests the on-chain cost of single deposits.
func TestOnchainCosts(t *testing.T) {
	txA := fundingTx(1, otherOut(700_000), staticOut(300_000))
	txShared := fundingTx(2, staticOut(300_000), staticOut(100_000))
	txExternal := fundingTx(3, staticOut(500_000))
	txMissing := fundingTx(4, staticOut(100_000))
	txEmpty := fundingTx(5, staticOut(0))
	txNegative := fundingTx(6, staticOut(100_000))

	getter := newFakeTxGetter(
		walletTx(txA, 1_550, true), walletTx(txShared, 1_500, true),
		walletTx(txExternal, 2_000, false),
		walletTx(txEmpty, 500, true), walletTx(txNegative, -1, true),
	)
	isDeposit := knownDeposits(
		txA, txShared, txExternal, txMissing, txEmpty, txNegative,
	)

	tests := []struct {
		name    string
		deposit *Deposit
		cost    OnchainCost
	}{
		{
			name:    "funded by the wallet",
			deposit: depositOf(txA, 1),
			cost:    knownCost(1_550),
		},
		{
			name:    "one of two deposits of a transaction",
			deposit: depositOf(txShared, 1),
			cost:    knownCost(375),
		},
		{
			name:    "funded by an external wallet",
			deposit: depositOf(txExternal, 0),
			cost:    unknownCost,
		},
		{
			name:    "funding transaction not in the wallet",
			deposit: depositOf(txMissing, 0),
			cost:    unknownCost,
		},
		{
			name:    "deposit outputs have no value",
			deposit: depositOf(txEmpty, 0),
			cost:    unknownCost,
		},
		{
			name:    "negative fee",
			deposit: depositOf(txNegative, 0),
			cost:    unknownCost,
		},
		{
			name: "output index out of range",
			deposit: &Deposit{
				OutPoint: wire.OutPoint{
					Hash: txA.TxHash(), Index: 5,
				},
			},
			cost: unknownCost,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			costs, err := onchainCosts(
				t.Context(), getter, isDeposit,
				[]*Deposit{tc.deposit},
			)
			require.NoError(t, err)
			require.Equal(t, map[wire.OutPoint]OnchainCost{
				tc.deposit.OutPoint: tc.cost,
			}, costs)
		})
	}
}

// TestOnchainCostsSiblings tests that the deposits of one funding transaction
// share its fee by value, also if they are looked up at different times or
// went to different static addresses, so the fee is counted once.
func TestOnchainCostsSiblings(t *testing.T) {
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
			getter := newFakeTxGetter(walletTx(tc.tx, 1_001, true))
			isDeposit := knownDeposits(tc.tx)
			first := depositOf(tc.tx, 0)
			second := depositOf(tc.tx, 2)

			// Both deposits at once: one lookup of the funding
			// transaction.
			both, err := onchainCosts(
				t.Context(), getter, isDeposit,
				[]*Deposit{first, second},
			)
			require.NoError(t, err)
			require.Equal(t, 1, getter.calls)
			require.Equal(t, map[wire.OutPoint]OnchainCost{
				first.OutPoint:  knownCost(626),
				second.OutPoint: knownCost(375),
			}, both)

			// One at a time gives the same shares.
			one, err := onchainCosts(
				t.Context(), getter, isDeposit,
				[]*Deposit{second},
			)
			require.NoError(t, err)
			require.Equal(t, knownCost(375), one[second.OutPoint])
		})
	}

	// A sibling to the same address that isn't a known deposit yet still
	// gets its share, so the first deposit isn't charged the whole fee.
	tx := fundingTx(
		0, staticOut(100_000), otherOut(50_000), staticOut(100_000),
	)
	getter := newFakeTxGetter(walletTx(tx, 1_001, true))
	first := depositOf(tx, 0)
	costs, err := onchainCosts(
		t.Context(), getter, func(outpoint wire.OutPoint) bool {
			return outpoint == first.OutPoint
		}, []*Deposit{first},
	)
	require.NoError(t, err)
	require.Equal(t, knownCost(501), costs[first.OutPoint])
}

// TestOnchainCostsChange tests that a deposit created as change by a
// transaction that spent earlier deposits, like a partial loop-in or a partial
// withdrawal, costs nothing, since it brought in no new funds.
func TestOnchainCostsChange(t *testing.T) {
	funding := fundingTx(1, otherOut(10_000), staticOut(1_000_000))
	fundingDeposit := depositOf(funding, 1)
	foreign := wire.OutPoint{Hash: chainhash.Hash{0xff}, Index: 3}

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
			// The server batches the deposits of several clients,
			// so not all inputs are ours.
			name:  "change of a batched spend",
			spend: spendTx(foreign),
			ours:  []bool{false, false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getter := newFakeTxGetter(
				walletTx(funding, 1_500, true),
				walletTx(tc.spend, 2_000, tc.ours...),
			)
			change := depositOf(tc.spend, 1)

			costs, err := onchainCosts(
				t.Context(), getter,
				knownDeposits(funding, tc.spend),
				[]*Deposit{change},
			)
			require.NoError(t, err)
			require.Equal(t, knownCost(0), costs[change.OutPoint])
		})
	}
}

// TestOnchainCostsLookupError tests that the lookups stop at the first
// funding transaction that can't be looked up, and that the costs found until
// then are returned.
func TestOnchainCostsLookupError(t *testing.T) {
	txExternal := fundingTx(1, staticOut(200_000))
	txFailing := fundingTx(2, staticOut(100_000))
	txWallet := fundingTx(3, staticOut(300_000), otherOut(5_000))

	getter := newFakeTxGetter(
		walletTx(txExternal, 900, false), walletTx(txWallet, 700, true),
	)
	getter.errs[txFailing.TxHash()] = errors.New("wallet unavailable")

	external := depositOf(txExternal, 0)
	failing := depositOf(txFailing, 0)
	wallet := depositOf(txWallet, 0)
	isDeposit := knownDeposits(txExternal, txFailing, txWallet)

	costs, err := onchainCosts(
		t.Context(), getter, isDeposit,
		[]*Deposit{external, failing, wallet},
	)
	require.ErrorContains(t, err, txFailing.TxHash().String())
	require.ErrorContains(t, err, "wallet unavailable")
	require.Equal(t, 2, getter.calls)
	require.Equal(t, map[wire.OutPoint]OnchainCost{
		external.OutPoint: unknownCost,
	}, costs)

	// Once lnd answers again, the other deposits get their cost.
	delete(getter.errs, txFailing.TxHash())
	getter.txs[txFailing.TxHash()] = walletTx(txFailing, 400, true)
	costs, err = onchainCosts(
		t.Context(), getter, isDeposit, []*Deposit{failing, wallet},
	)
	require.NoError(t, err)
	require.Equal(t, map[wire.OutPoint]OnchainCost{
		failing.OutPoint: knownCost(400),
		wallet.OutPoint:  knownCost(700),
	}, costs)
}

// TestOnchainCostsShutdown tests that the lookups stop when loopd shuts down
// during a lookup, and that the costs found before are returned.
func TestOnchainCostsShutdown(t *testing.T) {
	txWallet := fundingTx(1, staticOut(300_000), otherOut(5_000))
	txHanging := fundingTx(2, staticOut(200_000))
	txLater := fundingTx(3, staticOut(100_000))

	getter := newFakeTxGetter(
		walletTx(txWallet, 700, true), walletTx(txLater, 400, true),
	)
	getter.block[txHanging.TxHash()] = struct{}{}

	wallet := depositOf(txWallet, 0)
	hanging := depositOf(txHanging, 0)
	later := depositOf(txLater, 0)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	costs, err := onchainCosts(
		ctx, getter, knownDeposits(txWallet, txHanging, txLater),
		[]*Deposit{wallet, hanging, later},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 2, getter.calls)
	require.Equal(t, map[wire.OutPoint]OnchainCost{
		wallet.OutPoint: knownCost(700),
	}, costs)
}
