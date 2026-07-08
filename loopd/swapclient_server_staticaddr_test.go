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
	"github.com/lightninglabs/loop/staticaddr/script"
	mock_lnd "github.com/lightninglabs/loop/test"
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

// newTestDepositManager creates a deposit manager backed by seeded deposits.
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
