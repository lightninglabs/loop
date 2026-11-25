package deposit

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// txGetterWalletKit is a wallet kit that looks up transactions with a
// fakeTxGetter and returns a fixed sweep address.
type txGetterWalletKit struct {
	lndclient.WalletKitClient

	getter *fakeTxGetter
}

// GetTransaction looks up the transaction with the fake getter.
func (w *txGetterWalletKit) GetTransaction(ctx context.Context,
	txid chainhash.Hash) (lndclient.Transaction, error) {

	return w.getter.GetTransaction(ctx, txid)
}

// NextAddr returns a fixed taproot address.
func (w *txGetterWalletKit) NextAddr(context.Context, string,
	walletrpc.AddressType, bool) (btcutil.Address, error) {

	return btcutil.NewAddressTaproot(
		make([]byte, 32), &chaincfg.RegressionNetParams,
	)
}

// newOnchainCostStore returns a deposit store backed by a test database.
func newOnchainCostStore(t *testing.T) *SqlStore {
	t.Helper()

	testDb := loopdb.NewTestDB(t)
	t.Cleanup(func() {
		require.NoError(t, testDb.Close())
	})

	return NewSqlStore(testDb.BaseDB)
}

// newOnchainCostManager returns a manager that looks up transactions with the
// given getter and gets lnd's info from the given client.
func newOnchainCostManager(t *testing.T, store *SqlStore, getter *fakeTxGetter,
	lightningClient LightningClient) *Manager {

	t.Helper()

	addrParams := newOnchainCostAddress(t, store)
	addressManager := new(mockAddressManager)
	addressManager.On("GetParameters", testStaticPkScript).Return(addrParams)
	addressManager.On("GetParameters", testOtherPkScript).Return(nil)

	otherParams := *addrParams
	otherParams.ID = 0
	otherParams.PkScript = testStaticPkScript2
	otherParams.KeyLocator.Index++
	addressStore := address.NewSqlStore(store.baseDB)
	require.NoError(t, addressStore.CreateStaticAddress(t.Context(), &otherParams))
	otherID, err := addressStore.GetStaticAddressID(
		t.Context(), otherParams.PkScript,
	)
	require.NoError(t, err)
	otherParams.ID = otherID
	addressManager.On("GetParameters", testStaticPkScript2).Return(&otherParams)

	return NewManager(&ManagerConfig{
		LightningClient: lightningClient,
		AddressManager:  addressManager,
		Store:           store,
		WalletKit:       &txGetterWalletKit{getter: getter},
	})
}

// storeDeposit stores the given deposit, as if it had been found before.
func storeDeposit(t *testing.T, store *SqlStore, d *Deposit) {
	t.Helper()

	d.SetState(Deposited)
	d.TimeOutSweepPkScript = []byte{0x00, 0x14, 0x01}
	d.AddressParams = newOnchainCostAddress(t, store)
	require.NoError(t, store.CreateDeposit(t.Context(), d))
}

// storedCost returns the on-chain cost of the deposit in the store.
func storedCost(t *testing.T, store *SqlStore, d *Deposit) OnchainCost {
	t.Helper()

	loaded, err := store.GetDeposit(t.Context(), d.ID)
	require.NoError(t, err)

	return loaded.OnchainCost
}

// utxoOf returns the wallet utxo of the given output of a transaction.
func utxoOf(tx *wire.MsgTx, index uint32) *lnwallet.Utxo {
	return &lnwallet.Utxo{
		OutPoint: wire.OutPoint{Hash: tx.TxHash(), Index: index},
		Value:    btcutil.Amount(tx.TxOut[index].Value),
		PkScript: tx.TxOut[index].PkScript,
	}
}

// TestDetermineOnchainCosts tests that a new deposit is created with an
// undetermined cost, that the manager then determines its cost and stores it
// with the deposit, and that a cost that can't be determined yet stays
// undetermined.
func TestDetermineOnchainCosts(t *testing.T) {
	txWallet := fundingTx(1, staticOut(300_000), otherOut(10_000))
	txSiblings := fundingTx(
		2, staticOut(300_000), otherOut(10_000), staticOut(100_000),
	)
	txExternal := fundingTx(3, staticOut(500_000))
	txMissing := fundingTx(4, staticOut(200_000))
	txFailing := fundingTx(5, staticOut(200_000))

	// spend spends an earlier deposit and sends change back to the static
	// address.
	earlier := depositOf(fundingTx(6, staticOut(1_000_000)), 0)
	earlier.OnchainCost = knownCost(500)
	spend := wire.NewMsgTx(2)
	spend.AddTxIn(&wire.TxIn{PreviousOutPoint: earlier.OutPoint})
	spend.AddTxOut(otherOut(898_000))
	spend.AddTxOut(staticOut(100_000))

	unsynced := &testLightningClient{
		getInfo: func(context.Context) (*lndclient.Info, error) {
			return &lndclient.Info{}, nil
		},
	}

	tests := []struct {
		name     string
		tx       *wire.MsgTx
		index    uint32
		unsynced bool
		cost     OnchainCost
		calls    int
	}{
		{
			name:  "funded by the wallet",
			tx:    txWallet,
			cost:  knownCost(1_200),
			calls: 1,
		},
		{
			// The sibling at index 2 isn't stored yet, but gets
			// its share, so this deposit isn't charged the whole
			// fee.
			name:  "sibling not stored yet",
			tx:    txSiblings,
			cost:  knownCost(900),
			calls: 1,
		},
		{
			name:  "change of a spend of an earlier deposit",
			tx:    spend,
			index: 1,
			cost:  knownCost(0),
			calls: 1,
		},
		{
			name:  "funded by an external wallet",
			tx:    txExternal,
			cost:  unknownCost,
			calls: 1,
		},
		{
			name:  "funding tx not in the wallet",
			tx:    txMissing,
			cost:  unknownCost,
			calls: 1,
		},
		{
			name:  "lookup fails",
			tx:    txFailing,
			calls: 1,
		},
		{
			name:     "lnd not synced",
			tx:       txWallet,
			unsynced: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newOnchainCostStore(t)
			getter := newFakeTxGetter(
				walletTx(txWallet, 1_200, true),
				walletTx(txSiblings, 1_200, true),
				walletTx(spend, 2_000, false),
				walletTx(txExternal, 900, false),
			)
			getter.errs[txFailing.TxHash()] = errors.New(
				"wallet unavailable",
			)
			lightningClient := syncedTestLightningClient(100)
			if tc.unsynced {
				lightningClient = unsynced
			}
			m := newOnchainCostManager(
				t, store, getter, lightningClient,
			)

			storeDeposit(t, store, earlier)
			m.deposits[earlier.OutPoint] = earlier

			// The deposit is created without looking anything up.
			d, err := m.createNewDeposit(
				t.Context(), utxoOf(tc.tx, tc.index), 0,
			)
			require.NoError(t, err)
			require.Zero(t, getter.calls)
			require.Equal(t, OnchainCost{}, d.GetOnchainCost())
			require.Same(t, d, m.deposits[d.OutPoint])

			m.determineOnchainCosts(t.Context())
			require.Equal(t, tc.calls, getter.calls)
			require.Equal(t, tc.cost, d.GetOnchainCost())
			require.Equal(t, tc.cost, storedCost(t, store, d))
			require.Equal(t, knownCost(500), earlier.GetOnchainCost())
		})
	}
}

// TestDetermineOnchainCostsIncrementalDiscovery checks that a retained deposit
// is charged its final share before a sibling to another address is retained.
func TestDetermineOnchainCostsIncrementalDiscovery(t *testing.T) {
	tx := fundingTx(1, otherOut(500_000), staticOut(100_000),
		staticOut2(100_000))
	getter := newFakeTxGetter(walletTx(tx, 1_001, true))
	store := newOnchainCostStore(t)
	m := newOnchainCostManager(t, store, getter,
		syncedTestLightningClient(100))

	first, err := m.createNewDeposit(t.Context(), utxoOf(tx, 1), 0)
	require.NoError(t, err)
	m.determineOnchainCosts(t.Context())
	require.Equal(t, knownCost(501), storedCost(t, store, first))

	second, err := m.createNewDeposit(t.Context(), utxoOf(tx, 2), 0)
	require.NoError(t, err)
	m.determineOnchainCosts(t.Context())
	require.Equal(t, knownCost(501), storedCost(t, store, first))
	require.Equal(t, knownCost(500), storedCost(t, store, second))
	require.Equal(t, btcutil.Amount(1_001),
		first.GetOnchainCost().Amount+second.GetOnchainCost().Amount)
}

// TestDetermineOnchainCostsOneLookupPerTx tests that the deposits of one
// funding transaction look it up once and share its fee.
func TestDetermineOnchainCostsOneLookupPerTx(t *testing.T) {
	txSiblings := fundingTx(
		1, staticOut(300_000), otherOut(10_000), staticOut(100_000),
		staticOut(200_000),
	)
	txOther := fundingTx(2, staticOut(500_000), otherOut(10_000))
	getter := newFakeTxGetter(
		walletTx(txSiblings, 1_200, true), walletTx(txOther, 700, true),
	)
	m := newOnchainCostManager(
		t, newOnchainCostStore(t), getter, syncedTestLightningClient(100),
	)

	utxos := []*lnwallet.Utxo{
		utxoOf(txSiblings, 0), utxoOf(txOther, 0),
		utxoOf(txSiblings, 2), utxoOf(txSiblings, 3),
	}
	deposits := make([]*Deposit, 0, len(utxos))
	for _, utxo := range utxos {
		d, err := m.createNewDeposit(t.Context(), utxo, 0)
		require.NoError(t, err)
		deposits = append(deposits, d)
	}

	m.determineOnchainCosts(t.Context())
	require.Equal(t, 2, getter.calls)
	for i, cost := range []OnchainCost{
		knownCost(600), knownCost(700), knownCost(200), knownCost(400),
	} {
		require.Equal(t, cost, deposits[i].GetOnchainCost())
	}
}

// TestDetermineOnchainCostsRetry tests that the manager determines the on-chain cost of
// deposits that didn't get one when they were found, that it stops at a
// funding transaction that can't be looked up and tries it again later, and
// that it doesn't look up anything once all costs are determined.
func TestDetermineOnchainCostsRetry(t *testing.T) {
	store := newOnchainCostStore(t)

	txWallet := fundingTx(1, staticOut(300_000), otherOut(10_000))
	txFailing := fundingTx(2, staticOut(200_000))
	getter := newFakeTxGetter(walletTx(txWallet, 1_200, true))
	getter.errs[txFailing.TxHash()] = errors.New("wallet unavailable")
	m := newOnchainCostManager(
		t, store, getter, syncedTestLightningClient(100),
	)

	// Both deposits were found while lnd wasn't synced, and one already
	// has its cost.
	wallet, failing := depositOf(txWallet, 0), depositOf(txFailing, 0)
	known := depositOf(fundingTx(3, staticOut(100_000)), 0)
	known.OnchainCost = knownCost(500)
	for _, d := range []*Deposit{wallet, failing, known} {
		storeDeposit(t, store, d)
		m.deposits[d.OutPoint] = d
	}

	// The lookups stop at the failing funding transaction, so depending on
	// the order the wallet deposit may stay undetermined too. Retry until
	// it is determined.
	for range 10 {
		m.determineOnchainCosts(t.Context())
		if wallet.GetOnchainCost().Status != OnchainCostUndetermined {
			break
		}
	}
	require.Equal(t, knownCost(1_200), wallet.GetOnchainCost())
	require.Equal(t, knownCost(1_200), storedCost(t, store, wallet))
	require.Equal(t, OnchainCost{}, failing.GetOnchainCost())
	require.Equal(t, OnchainCost{}, storedCost(t, store, failing))
	require.Equal(t, knownCost(500), known.GetOnchainCost())

	// Once the funding transaction can be looked up, the next call
	// determines its cost and only looks up that one.
	delete(getter.errs, txFailing.TxHash())
	getter.txs[txFailing.TxHash()] = walletTx(txFailing, 400, true)
	getter.calls = 0
	m.determineOnchainCosts(t.Context())
	require.Equal(t, 1, getter.calls)
	require.Equal(t, knownCost(400), failing.GetOnchainCost())
	require.Equal(t, knownCost(400), storedCost(t, store, failing))

	// With nothing left, it doesn't look anything up.
	getter.calls = 0
	m.determineOnchainCosts(t.Context())
	require.Zero(t, getter.calls)
}

// failingCostStore is a deposit store whose on-chain cost writes fail.
type failingCostStore struct {
	*SqlStore
}

// SetOnchainCosts fails.
func (s *failingCostStore) SetOnchainCosts(context.Context,
	map[ID]OnchainCost) error {

	return errors.New("database unavailable")
}

// TestDetermineOnchainCostsNotDetermined tests that a deposit's cost stays
// undetermined, in memory and in the store, if lnd's info is unavailable, if
// storing the cost fails, or if loopd is shutting down.
func TestDetermineOnchainCostsNotDetermined(t *testing.T) {
	tx := fundingTx(1, staticOut(300_000), otherOut(10_000))

	infoErr := &testLightningClient{
		getInfo: func(context.Context) (*lndclient.Info, error) {
			return nil, errors.New("lnd unavailable")
		},
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name            string
		lightningClient LightningClient
		failStore       bool
		ctx             context.Context
	}{
		{
			name:            "lnd info unavailable",
			lightningClient: infoErr,
			ctx:             t.Context(),
		},
		{
			name:            "storing fails",
			lightningClient: syncedTestLightningClient(100),
			failStore:       true,
			ctx:             t.Context(),
		},
		{
			name:            "shutting down",
			lightningClient: syncedTestLightningClient(100),
			ctx:             canceled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sqlStore := newOnchainCostStore(t)
			var store Store = sqlStore
			if tc.failStore {
				store = &failingCostStore{SqlStore: sqlStore}
			}
			getter := newFakeTxGetter(walletTx(tx, 1_200, true))
			m := newOnchainCostManager(
				t, sqlStore, getter, tc.lightningClient,
			)
			m.cfg.Store = store

			d := depositOf(tx, 0)
			storeDeposit(t, sqlStore, d)
			m.deposits[d.OutPoint] = d

			m.determineOnchainCosts(tc.ctx)
			require.Equal(t, OnchainCost{}, d.GetOnchainCost())
			require.Equal(t, OnchainCost{}, storedCost(t, sqlStore, d))
		})
	}
}
