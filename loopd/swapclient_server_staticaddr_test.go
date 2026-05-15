package loopd

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	mock_lnd "github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
)

type staticAddrDepositStore struct {
	allDeposits []*deposit.Deposit
	byOutpoint  map[string]*deposit.Deposit
}

func (s *staticAddrDepositStore) CreateDeposit(context.Context,
	*deposit.Deposit) error {

	return nil
}

func (s *staticAddrDepositStore) UpdateDeposit(context.Context,
	*deposit.Deposit) error {

	return nil
}

func (s *staticAddrDepositStore) GetDeposit(context.Context,
	deposit.ID) (*deposit.Deposit, error) {

	return nil, nil
}

func (s *staticAddrDepositStore) DepositForOutpoint(_ context.Context,
	outpoint string) (*deposit.Deposit, error) {

	if deposit, ok := s.byOutpoint[outpoint]; ok {
		return deposit, nil
	}

	return nil, deposit.ErrDepositNotFound
}

func (s *staticAddrDepositStore) AllDeposits(context.Context) (
	[]*deposit.Deposit, error) {

	return s.allDeposits, nil
}

func newTestDepositManager(
	deposits ...*deposit.Deposit) *deposit.Manager {

	byOutpoint := make(map[string]*deposit.Deposit, len(deposits))
	for _, deposit := range deposits {
		byOutpoint[deposit.OutPoint.String()] = deposit
	}

	return deposit.NewManager(&deposit.ManagerConfig{
		Store: &staticAddrDepositStore{
			allDeposits: deposits,
			byOutpoint:  byOutpoint,
		},
	})
}

func newTestStaticAddressContext(t *testing.T) (*address.Manager,
	*mock_lnd.LndMockServices) {

	t.Helper()

	mock := mock_lnd.NewMockLnd()
	_, client := mock_lnd.CreateKey(1)
	_, server := mock_lnd.CreateKey(2)

	addrStore := &mockAddressStore{
		params: []*address.Parameters{{
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

func TestListStaticAddressDepositsHidesReplaced(t *testing.T) {
	t.Parallel()

	replaced := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 1,
		},
	}
	replaced.SetState(deposit.Replaced)

	available := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{2},
			Index: 2,
		},
	}
	available.SetState(deposit.Deposited)

	addrMgr, lnd := newTestStaticAddressContext(t)
	server := &swapClientServer{
		depositManager:       newTestDepositManager(replaced, available),
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

func TestGetStaticAddressSummaryIgnoresReplaced(t *testing.T) {
	t.Parallel()

	replaced := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{3},
			Index: 3,
		},
		Value: btcutil.Amount(1_000),
	}
	replaced.SetState(deposit.Replaced)

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
			replaced, unconfirmed, confirmed,
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
