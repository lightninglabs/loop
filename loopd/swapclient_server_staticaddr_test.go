package loopd

import (
	"context"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	mock_lnd "github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

type staticAddrDepositStore struct {
	allDeposits []*deposit.Deposit
	byOutpoint  map[string]*deposit.Deposit
}

// CreateDeposit implements deposit.Store for static address server tests.
func (s *staticAddrDepositStore) CreateDeposit(context.Context,
	*deposit.Deposit) error {

	return nil
}

// UpdateDeposit implements deposit.Store for static address server tests.
func (s *staticAddrDepositStore) UpdateDeposit(context.Context,
	*deposit.Deposit) error {

	return nil
}

// GetDeposit implements deposit.Store for static address server tests.
func (s *staticAddrDepositStore) GetDeposit(context.Context,
	deposit.ID) (*deposit.Deposit, error) {

	return nil, nil
}

// DepositForOutpoint returns the deposit for the requested outpoint.
func (s *staticAddrDepositStore) DepositForOutpoint(_ context.Context,
	outpoint string) (*deposit.Deposit, error) {

	if deposit, ok := s.byOutpoint[outpoint]; ok {
		return deposit, nil
	}

	return nil, deposit.ErrDepositNotFound
}

// AllDeposits returns all deposits seeded into the test store.
func (s *staticAddrDepositStore) AllDeposits(context.Context) (
	[]*deposit.Deposit, error) {

	return s.allDeposits, nil
}

type staticAddrTestAddressManager struct {
	params *address.Parameters
}

func newStaticAddrTestAddressManager() *staticAddrTestAddressManager {
	_, client := mock_lnd.CreateKey(1)
	_, server := mock_lnd.CreateKey(2)

	return &staticAddrTestAddressManager{
		params: &address.Parameters{
			ID:           1,
			ClientPubkey: client,
			ServerPubkey: server,
			Expiry:       10,
			PkScript:     []byte("pkscript"),
		},
	}
}

func (s *staticAddrTestAddressManager) GetStaticAddressParameters(
	context.Context) (*script.Parameters, error) {

	return s.params, nil
}

func (s *staticAddrTestAddressManager) GetStaticAddressID(
	context.Context, []byte) (int32, error) {

	return s.params.ID, nil
}

func (s *staticAddrTestAddressManager) GetParameters(
	pkScript []byte) *address.Parameters {

	params := *s.params
	params.PkScript = pkScript

	return &params
}

func (s *staticAddrTestAddressManager) GetStaticAddress(
	context.Context) (*script.StaticAddress, error) {

	return nil, nil
}

func (s *staticAddrTestAddressManager) ListUnspent(context.Context,
	int32, int32) ([]*lnwallet.Utxo, error) {

	return nil, nil
}

func (s *staticAddrTestAddressManager) GetTaprootAddress(
	*btcec.PublicKey, *btcec.PublicKey, int64) (*btcutil.AddressTaproot,
	error) {

	return nil, nil
}

// newTestDepositManager creates a deposit manager backed by seeded deposits.
func newTestDepositManager(
	deposits ...*deposit.Deposit) *deposit.Manager {

	byOutpoint := make(map[string]*deposit.Deposit, len(deposits))
	for _, deposit := range deposits {
		byOutpoint[deposit.OutPoint.String()] = deposit
	}

	return deposit.NewManager(&deposit.ManagerConfig{
		AddressManager: newStaticAddrTestAddressManager(),
		Store: &staticAddrDepositStore{
			allDeposits: deposits,
			byOutpoint:  byOutpoint,
		},
	})
}

// newTestStaticAddressContext creates static address test dependencies.
func newTestStaticAddressContext(t *testing.T) (*address.Manager,
	*mock_lnd.LndMockServices) {

	t.Helper()

	mock := mock_lnd.NewMockLnd()
	_, client := mock_lnd.CreateKey(1)
	_, server := mock_lnd.CreateKey(2)

	addrStore := &mockAddressStore{
		params: []*script.Parameters{{
			ClientPubkey: client,
			ServerPubkey: server,
			Expiry:       10,
			PkScript:     []byte("pkscript"),
		}},
	}

	addrMgr, err := address.NewManager(&address.ManagerConfig{
		Store:       addrStore,
		WalletKit:   mock.WalletKit,
		ChainParams: mock.ChainParams,
	}, 1)
	require.NoError(t, err)

	return addrMgr, mock
}

func TestValidateStaticAddressSendCoinsRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  *lnrpc.SendCoinsRequest
		err  string
	}{
		{
			name: "nil",
		},
		{
			name: "amount",
			req: &lnrpc.SendCoinsRequest{
				Amount: 10_000,
			},
		},
		{
			name: "send all",
			req: &lnrpc.SendCoinsRequest{
				SendAll: true,
			},
		},
		{
			name: "existing addr",
			req: &lnrpc.SendCoinsRequest{
				Addr:   "bcrt1ptestaddress",
				Amount: 10_000,
			},
		},
		{
			name: "missing amount",
			req:  &lnrpc.SendCoinsRequest{},
			err:  "must set amount or send_all",
		},
		{
			name: "negative amount",
			req: &lnrpc.SendCoinsRequest{
				Amount: -1,
			},
			err: "amount must be non-negative",
		},
		{
			name: "amount and send all",
			req: &lnrpc.SendCoinsRequest{
				Amount:  10_000,
				SendAll: true,
			},
			err: "amount cannot be set when send_all is true",
		},
		{
			name: "target and fee rate",
			req: &lnrpc.SendCoinsRequest{
				Amount:           10_000,
				TargetConf:       6,
				SatPerVbyte:      1,
				SatPerByte:       0,
				SendAll:          false,
				MinConfs:         1,
				Outpoints:        nil,
				SpendUnconfirmed: false,
			},
			err: "can set either target_conf or a fee rate",
		},
		{
			name: "both fee rates",
			req: &lnrpc.SendCoinsRequest{
				Amount:      10_000,
				SatPerVbyte: 1,
				SatPerByte:  1,
			},
			err: "can set either sat_per_vbyte or sat_per_byte",
		},
		{
			name: "negative min confs",
			req: &lnrpc.SendCoinsRequest{
				Amount:   10_000,
				MinConfs: -1,
			},
			err: "min_confs must be non-negative",
		},
		{
			name: "min confs with spend unconfirmed",
			req: &lnrpc.SendCoinsRequest{
				Amount:           10_000,
				MinConfs:         1,
				SpendUnconfirmed: true,
			},
			err: "spend_unconfirmed invalid",
		},
		{
			name: "invalid label",
			req: &lnrpc.SendCoinsRequest{
				Amount: 10_000,
				Label:  strings.Repeat("x", 501),
			},
			err: "label invalid",
		},
		{
			name: "invalid coin selection strategy",
			req: &lnrpc.SendCoinsRequest{
				Amount:                10_000,
				CoinSelectionStrategy: lnrpc.CoinSelectionStrategy(99),
			},
			err: "coin_selection_strategy invalid",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateStaticAddressSendCoinsRequest(test.req)
			if test.err == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.err)
		})
	}
}

func TestStaticAddressForDeposit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addrMgr, _ := newTestStaticAddressContext(t)
	server := &swapClientServer{
		staticAddressManager: addrMgr,
	}

	addresses, err := addrMgr.GetAllAddresses(ctx)
	require.NoError(t, err)
	require.Len(t, addresses, 1)

	expectedAddr, err := addrMgr.GetTaprootAddress(
		addresses[0].ClientPubkey, addresses[0].ServerPubkey,
		int64(addresses[0].Expiry),
	)
	require.NoError(t, err)

	addr, expiry, err := server.staticAddressForDeposit(
		ctx, expectedAddr.String(),
	)
	require.NoError(t, err)
	require.Equal(t, expectedAddr.String(), addr)
	require.Equal(t, addresses[0].Expiry, expiry)

	_, _, err = server.staticAddressForDeposit(
		ctx, "bcrt1punknownstaticaddress",
	)
	require.ErrorContains(t, err, "not a known static address")
}

// TestListStaticAddressDepositsReturnsVisibleDeposits verifies normal deposit
// listings include visible deposit records.
func TestListStaticAddressDepositsReturnsVisibleDeposits(t *testing.T) {
	t.Parallel()

	available := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{2},
			Index: 2,
		},
	}
	available.SetState(deposit.Deposited)

	addrMgr, lnd := newTestStaticAddressContext(t)
	server := &swapClientServer{
		depositManager:       newTestDepositManager(available),
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}

	resp, err := server.ListStaticAddressDeposits(
		context.Background(), &looprpc.ListStaticAddressDepositsRequest{},
	)
	require.NoError(t, err)
	require.Len(t, resp.FilteredDeposits, 1)
	require.Equal(
		t, available.OutPoint.String(),
		resp.FilteredDeposits[0].Outpoint,
	)
}

// TestGetStaticAddressSummaryTotalsDeposits verifies visible deposits are
// included in static address summary totals.
func TestGetStaticAddressSummaryTotalsDeposits(t *testing.T) {
	t.Parallel()

	unconfirmed := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{4},
			Index: 4,
		},
		Value:              btcutil.Amount(2_000),
		ConfirmationHeight: 0,
	}
	unconfirmed.SetState(deposit.Deposited)

	confirmed := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{5},
			Index: 5,
		},
		Value:              btcutil.Amount(3_000),
		ConfirmationHeight: 123,
	}
	confirmed.SetState(deposit.Deposited)

	addrMgr, _ := newTestStaticAddressContext(t)
	server := &swapClientServer{
		depositManager: newTestDepositManager(
			unconfirmed, confirmed,
		),
		staticAddressManager: addrMgr,
	}

	resp, err := server.GetStaticAddressSummary(
		context.Background(), &looprpc.StaticAddressSummaryRequest{},
	)
	require.NoError(t, err)
	require.EqualValues(t, 2, resp.TotalNumDeposits)
	require.EqualValues(t, 2_000, resp.ValueUnconfirmedSatoshis)
	require.EqualValues(t, 3_000, resp.ValueDepositedSatoshis)
}

// TestGetLoopInQuoteRejectsUnavailableSelectedDeposit verifies manual quote
// requests fail for selected deposits that are no longer available.
func TestGetLoopInQuoteRejectsUnavailableSelectedDeposit(t *testing.T) {
	t.Parallel()
	setLogger(btclog.Disabled)

	locked := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{6},
			Index: 6,
		},
		Value: btcutil.Amount(5_000),
	}
	locked.SetState(deposit.LoopingIn)

	addrMgr, lnd := newTestStaticAddressContext(t)
	server := &swapClientServer{
		depositManager:       newTestDepositManager(locked),
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}

	_, err := server.GetLoopInQuote(context.Background(), &looprpc.QuoteRequest{
		DepositOutpoints: []string{locked.OutPoint.String()},
	})
	require.ErrorContains(t, err, "is not currently available")
}
