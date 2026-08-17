package loopd

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	mock_lnd "github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

type staticAddrTestLightningClient struct {
	lndclient.LightningClient
}

func (c *staticAddrTestLightningClient) GetInfo(context.Context) (
	*lndclient.Info, error) {

	return &lndclient.Info{
		BlockHeight:   1,
		BestBlockHash: chainhash.Hash{1},
		SyncedToChain: true,
	}, nil
}

// staticAddrTestLoopInQuoter records the Loop In quote request it receives.
type staticAddrTestLoopInQuoter struct {
	request *loop.LoopInQuoteRequest
}

// LoopInQuote records the request and returns an empty quote.
func (q *staticAddrTestLoopInQuoter) LoopInQuote(_ context.Context,
	request *loop.LoopInQuoteRequest) (*loop.LoopInQuote, error) {

	q.request = request

	return &loop.LoopInQuote{}, nil
}

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

type staticAddrTestAddressManager struct{}

func (s *staticAddrTestAddressManager) GetStaticAddressParameters(
	context.Context) (*script.Parameters, error) {

	return nil, nil
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
		LightningClient: &staticAddrTestLightningClient{},
		AddressManager:  &staticAddrTestAddressManager{},
		Store: &staticAddrDepositStore{
			allDeposits: deposits,
			byOutpoint:  byOutpoint,
		},
	})
}

// newTestStaticAddressContext creates static address test dependencies.
func newTestStaticAddressContext(t *testing.T, expiry uint32) (*address.Manager,
	*mock_lnd.LndMockServices) {

	t.Helper()

	mock := mock_lnd.NewMockLnd()
	_, client := mock_lnd.CreateKey(1)
	_, server := mock_lnd.CreateKey(2)

	addrStore := &mockAddressStore{
		params: []*script.Parameters{{
			ClientPubkey: client,
			ServerPubkey: server,
			Expiry:       expiry,
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

	addrMgr, lnd := newTestStaticAddressContext(t, 10)
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

	addrMgr, _ := newTestStaticAddressContext(t, 10)
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

	addrMgr, lnd := newTestStaticAddressContext(t, 10)
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

// TestGetLoopInQuoteRejectsExpiringSelectedDeposit verifies manual quote
// requests fail before server quote retrieval when a selected deposit no longer
// has enough timeout runway for a static-address loop-in HTLC.
func TestGetLoopInQuoteRejectsExpiringSelectedDeposit(t *testing.T) {
	t.Parallel()
	setLogger(btclog.Disabled)

	expiring := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{7},
			Index: 7,
		},
		Value:              btcutil.Amount(5_000),
		ConfirmationHeight: 500,
	}
	expiring.SetState(deposit.Deposited)

	addrMgr, lnd := newTestStaticAddressContext(t, 10)
	server := &swapClientServer{
		depositManager:       newTestDepositManager(expiring),
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}

	_, err := server.GetLoopInQuote(t.Context(), &looprpc.QuoteRequest{
		DepositOutpoints: []string{expiring.OutPoint.String()},
	})
	require.ErrorContains(t, err, "expires before htlc")
}

// TestGetLoopInQuoteAllowsFreshSelectedDeposit verifies the static address
// expiry and current height are passed to manual quote validation in the
// correct order.
func TestGetLoopInQuoteAllowsFreshSelectedDeposit(t *testing.T) {
	t.Parallel()
	setLogger(btclog.Disabled)

	const (
		confirmationHeight = 500
		staticAddrExpiry   = 2_000
	)

	fresh := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{8},
			Index: 8,
		},
		Value:              btcutil.Amount(5_000),
		ConfirmationHeight: confirmationHeight,
	}
	fresh.SetState(deposit.Deposited)

	quoter := &staticAddrTestLoopInQuoter{}
	addrMgr, lnd := newTestStaticAddressContext(t, staticAddrExpiry)
	server := &swapClientServer{
		depositManager:       newTestDepositManager(fresh),
		staticAddressManager: addrMgr,
		loopInQuoter:         quoter,
		lnd:                  &lnd.LndServices,
	}

	response, err := server.GetLoopInQuote(t.Context(), &looprpc.QuoteRequest{
		DepositOutpoints: []string{fresh.OutPoint.String()},
	})
	require.NoError(t, err)
	require.NotNil(t, response)
	require.NotNil(t, quoter.request)
	require.Equal(t, fresh.Value, quoter.request.Amount)
	require.EqualValues(t, 1, quoter.request.NumDeposits)
}
