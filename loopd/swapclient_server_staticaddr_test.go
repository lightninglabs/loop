package loopd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/withdraw"
	mock_lnd "github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
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

type sendCoinsRPCClient struct {
	lnrpc.LightningClient

	request  *lnrpc.SendCoinsRequest
	response *lnrpc.SendCoinsResponse
	err      error
}

func (c *sendCoinsRPCClient) SendCoins(_ context.Context,
	req *lnrpc.SendCoinsRequest, _ ...grpc.CallOption) (
	*lnrpc.SendCoinsResponse, error) {

	c.request = proto.Clone(req).(*lnrpc.SendCoinsRequest)

	return c.response, c.err
}

type sendCoinsLightningClient struct {
	lndclient.LightningClient

	rawClient lnrpc.LightningClient
}

func (c *sendCoinsLightningClient) RawClientWithMacAuth(
	ctx context.Context) (context.Context, time.Duration,
	lnrpc.LightningClient) {

	return ctx, time.Second, c.rawClient
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

type staticAddrTestAddressManager struct {
	params *address.AddressParameters
}

func newStaticAddrTestAddressManager() *staticAddrTestAddressManager {
	_, client := mock_lnd.CreateKey(1)
	_, server := mock_lnd.CreateKey(2)

	return &staticAddrTestAddressManager{
		params: &address.AddressParameters{
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
	pkScript []byte) *address.AddressParameters {

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
		LightningClient: &staticAddrTestLightningClient{},
		AddressManager:  newStaticAddrTestAddressManager(),
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
	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(expiry), client, server,
	)
	require.NoError(t, err)
	pkScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)

	addrStore := &mockAddressStore{
		params: []*script.Parameters{{
			ClientPubkey: client,
			ServerPubkey: server,
			Expiry:       expiry,
			PkScript:     pkScript,
		}},
	}

	addrMgr, err := address.NewManager(&address.ManagerConfig{
		Store:         addrStore,
		WalletKit:     mock.WalletKit,
		ChainParams:   mock.ChainParams,
		ChainNotifier: mock.ChainNotifier,
	}, 1)
	require.NoError(t, err)

	initChan := make(chan struct{})
	go func() {
		_ = addrMgr.Run(t.Context(), initChan)
	}()
	select {
	case <-initChan:
	case <-t.Context().Done():
		t.Fatal("address manager initialization canceled")
	}

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
			err:  "send_coins_request is required",
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

// TestFundStaticAddressFundsGeneratedAddress verifies the RPC funds the derived
// address and forwards coin selection options without mutating the request.
func TestFundStaticAddressFundsGeneratedAddress(t *testing.T) {
	t.Parallel()

	addrMgr, lnd := newTestStaticAddressContext(t, 10)
	rawClient := &sendCoinsRPCClient{
		response: &lnrpc.SendCoinsResponse{Txid: "funding-txid"},
	}
	lnd.Client = &sendCoinsLightningClient{rawClient: rawClient}
	server := &swapClientServer{
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}

	sendCoinsReq := &lnrpc.SendCoinsRequest{
		Amount:                100_000,
		TargetConf:            6,
		Label:                 "static-address-deposit",
		MinConfs:              1,
		CoinSelectionStrategy: lnrpc.CoinSelectionStrategy_STRATEGY_RANDOM,
		Outpoints: []*lnrpc.OutPoint{{
			TxidStr:     strings.Repeat("01", 32),
			OutputIndex: 2,
		}},
	}
	resp, err := server.FundStaticAddress(
		t.Context(), &looprpc.FundStaticAddressRequest{
			SendCoinsRequest: sendCoinsReq,
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, resp.Address)
	require.Equal(t, "funding-txid", resp.GetSendCoinsResponse().GetTxid())

	expectedReq := proto.Clone(sendCoinsReq).(*lnrpc.SendCoinsRequest)
	expectedReq.Addr = resp.Address
	require.True(t, proto.Equal(expectedReq, rawClient.request))
	require.Empty(t, sendCoinsReq.Addr)
}

// TestNewStaticAddressDoesNotFund verifies address creation never invokes the
// wallet's SendCoins method, even with the removed funding field on the wire.
func TestNewStaticAddressDoesNotFund(t *testing.T) {
	t.Parallel()

	addrMgr, lnd := newTestStaticAddressContext(t, 10)
	rawClient := &sendCoinsRPCClient{}
	lnd.Client = &sendCoinsLightningClient{rawClient: rawClient}
	server := &swapClientServer{
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}

	// An old experimental client could still serialize funding as field 2.
	// Protobuf preserves it as unknown data, but the address RPC cannot spend.
	funding, err := proto.Marshal(&lnrpc.SendCoinsRequest{SendAll: true})
	require.NoError(t, err)
	encoded := protowire.AppendTag(nil, 2, protowire.BytesType)
	encoded = protowire.AppendBytes(encoded, funding)
	req := &looprpc.NewStaticAddressRequest{}
	require.NoError(t, proto.Unmarshal(encoded, req))

	first, err := server.NewStaticAddress(t.Context(), req)
	require.NoError(t, err)
	second, err := server.NewStaticAddress(
		t.Context(), &looprpc.NewStaticAddressRequest{},
	)
	require.NoError(t, err)
	require.NotEmpty(t, first.Address)
	require.NotEqual(t, first.Address, second.Address)
	require.EqualValues(t, 10, first.Expiry)
	require.Nil(t, rawClient.request)
}

// TestFundStaticAddressRejectsInvalidRequests verifies funding validation runs
// before any address manager or wallet access.
func TestFundStaticAddressRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	for _, req := range []*looprpc.FundStaticAddressRequest{
		nil,
		{},
		{SendCoinsRequest: &lnrpc.SendCoinsRequest{}},
		{SendCoinsRequest: &lnrpc.SendCoinsRequest{Amount: -1}},
		{SendCoinsRequest: &lnrpc.SendCoinsRequest{
			Amount: 1, SendAll: true,
		}},
	} {
		// Nil dependencies would panic if validation allowed a side effect.
		server := &swapClientServer{}
		resp, err := server.FundStaticAddress(t.Context(), req)
		require.Nil(t, resp)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
}

// TestFundStaticAddressExistingAddress verifies funding an existing address
// preserves send-all options and does not derive another address.
func TestFundStaticAddressExistingAddress(t *testing.T) {
	t.Parallel()

	addrMgr, lnd := newTestStaticAddressContext(t, 10)
	rawClient := &sendCoinsRPCClient{
		response: &lnrpc.SendCoinsResponse{Txid: "existing-funding-txid"},
	}
	lnd.Client = &sendCoinsLightningClient{rawClient: rawClient}
	server := &swapClientServer{
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}

	addr, err := addrMgr.GetStaticAddress(t.Context())
	require.NoError(t, err)
	encodedAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(addr.TaprootKey), lnd.ChainParams,
	)
	require.NoError(t, err)
	req := &lnrpc.SendCoinsRequest{
		Addr: encodedAddr.String(), SendAll: true, SatPerVbyte: 2,
	}
	resp, err := server.FundStaticAddress(
		t.Context(), &looprpc.FundStaticAddressRequest{
			SendCoinsRequest: req,
		},
	)
	require.NoError(t, err)
	require.Equal(t, req.Addr, resp.Address)
	require.EqualValues(t, 10, resp.Expiry)
	require.Equal(t, rawClient.response, resp.SendCoinsResponse)
	require.True(t, proto.Equal(req, rawClient.request))
	addresses, err := addrMgr.GetAllAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, addresses, 1)

	// A valid Bitcoin address outside the active static-address index must
	// never be forwarded to SendCoins.
	_, otherKey := mock_lnd.CreateKey(99)
	unknownAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(otherKey), lnd.ChainParams,
	)
	require.NoError(t, err)
	rawClient.request = nil
	req.Addr = unknownAddr.String()
	resp, err = server.FundStaticAddress(
		t.Context(), &looprpc.FundStaticAddressRequest{
			SendCoinsRequest: req,
		},
	)
	require.Nil(t, resp)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Nil(t, rawClient.request)
}

// TestFundStaticAddressWalletFailure verifies a failed broadcast preserves the
// newly created address and identifies it in the error for recovery.
func TestFundStaticAddressWalletFailure(t *testing.T) {
	t.Parallel()

	addrMgr, lnd := newTestStaticAddressContext(t, 10)
	walletErr := errors.New("wallet unavailable")
	rawClient := &sendCoinsRPCClient{err: walletErr}
	lnd.Client = &sendCoinsLightningClient{rawClient: rawClient}
	server := &swapClientServer{
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}
	resp, err := server.FundStaticAddress(
		t.Context(), &looprpc.FundStaticAddressRequest{
			SendCoinsRequest: &lnrpc.SendCoinsRequest{Amount: 100_000},
		},
	)
	require.Nil(t, resp)
	require.ErrorIs(t, err, walletErr)
	require.NotNil(t, rawClient.request)
	require.NotEmpty(t, rawClient.request.Addr)
	require.ErrorContains(t, err, rawClient.request.Addr)

	addresses, err := addrMgr.GetAllAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, addresses, 2)
	_, _, err = server.staticAddressForDeposit(
		t.Context(), rawClient.request.Addr,
	)
	require.NoError(t, err)
}

func TestStaticAddressForDeposit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	addrMgr, lnd := newTestStaticAddressContext(t, 10)
	server := &swapClientServer{
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
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

	addrMgr, lnd := newTestStaticAddressContext(t, 10)
	addresses, err := addrMgr.GetAllAddresses(context.Background())
	require.NoError(t, err)
	require.Len(t, addresses, 1)
	available.AddressParams = addresses[0]

	expectedAddr, err := addrMgr.GetTaprootAddress(
		addresses[0].ClientPubkey, addresses[0].ServerPubkey,
		int64(addresses[0].Expiry),
	)
	require.NoError(t, err)

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
	require.Equal(
		t, expectedAddr.String(),
		resp.FilteredDeposits[0].StaticAddress,
	)
}

// TestStaticAddressWithdrawalIncludesDepositAddress verifies withdrawal
// listings use the common deposit conversion path, including the address that
// received each deposit.
func TestStaticAddressWithdrawalIncludesDepositAddress(t *testing.T) {
	t.Parallel()

	addrMgr, _ := newTestStaticAddressContext(t, 10)
	addresses, err := addrMgr.GetAllAddresses(context.Background())
	require.NoError(t, err)
	require.Len(t, addresses, 1)

	expectedAddr, err := addrMgr.GetTaprootAddress(
		addresses[0].ClientPubkey, addresses[0].ServerPubkey,
		int64(addresses[0].Expiry),
	)
	require.NoError(t, err)

	d := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{3},
			Index: 3,
		},
		AddressParams: addresses[0],
	}
	d.SetState(deposit.Withdrawn)

	server := &swapClientServer{
		staticAddressManager: addrMgr,
	}
	rpcWithdrawal, err := server.rpcStaticAddressWithdrawal(
		withdraw.Withdrawal{
			Deposits: []*deposit.Deposit{d},
		},
	)
	require.NoError(t, err)
	require.Len(t, rpcWithdrawal.Deposits, 1)
	require.Equal(
		t, expectedAddr.String(),
		rpcWithdrawal.Deposits[0].StaticAddress,
	)
}

func TestRPCDepositRequiresAddressParams(t *testing.T) {
	t.Parallel()

	d := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{7},
			Index: 7,
		},
	}

	server := &swapClientServer{}
	rpcDeposit, err := server.rpcDeposit(d)
	require.Nil(t, rpcDeposit)
	require.ErrorContains(
		t, err, "missing static address parameters for deposit "+
			d.OutPoint.String(),
	)
}

func TestPopulateBlocksUntilExpiryUsesOwningAddress(t *testing.T) {
	t.Parallel()

	const confirmationHeight = int64(590)
	first := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{8},
			Index: 8,
		},
		ConfirmationHeight: confirmationHeight,
		AddressParams: &script.Parameters{
			Expiry: 20,
		},
	}
	second := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{9},
			Index: 9,
		},
		ConfirmationHeight: confirmationHeight,
		AddressParams: &script.Parameters{
			Expiry: 40,
		},
	}
	rpcDeposits := []*looprpc.Deposit{
		{
			Outpoint:           first.OutPoint.String(),
			ConfirmationHeight: confirmationHeight,
		},
		{
			Outpoint:           second.OutPoint.String(),
			ConfirmationHeight: confirmationHeight,
		},
	}

	lnd := mock_lnd.NewMockLnd()
	server := &swapClientServer{lnd: &lnd.LndServices}
	err := server.populateBlocksUntilExpiry(
		t.Context(), []*deposit.Deposit{first, second}, rpcDeposits,
	)
	require.NoError(t, err)
	require.EqualValues(t, 10, rpcDeposits[0].BlocksUntilExpiry)
	require.EqualValues(t, 30, rpcDeposits[1].BlocksUntilExpiry)
}

func TestStaticAddressLoopInResponseIncludesDepositAddress(t *testing.T) {
	t.Parallel()

	addrMgr, lnd := newTestStaticAddressContext(t, 10)
	addresses, err := addrMgr.GetAllAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, addresses, 1)

	d := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{10},
			Index: 10,
		},
		Value:              100_000,
		ConfirmationHeight: 590,
		AddressParams:      addresses[0],
	}
	d.SetState(deposit.LoopingIn)
	loopIn := &loopin.StaticAddressLoopIn{
		SwapHash: lntypes.Hash{10},
		Deposits: []*deposit.Deposit{d},
	}

	server := &swapClientServer{
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}
	resp, err := server.rpcStaticAddressLoopInResponse(
		t.Context(), loopIn,
	)
	require.NoError(t, err)
	require.Len(t, resp.UsedDeposits, 1)

	expectedAddr, err := addrMgr.GetTaprootAddress(
		addresses[0].ClientPubkey, addresses[0].ServerPubkey,
		int64(addresses[0].Expiry),
	)
	require.NoError(t, err)
	require.Equal(
		t, expectedAddr.String(), resp.UsedDeposits[0].StaticAddress,
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

// TestGetStaticAddressSummaryNoAddress verifies a missing static address root
// is exposed as a durable gRPC status instead of an application error encoded
// in an Unknown status.
func TestGetStaticAddressSummaryNoAddress(t *testing.T) {
	t.Parallel()

	addrMgr, err := address.NewManager(&address.ManagerConfig{
		Store: &mockAddressStore{},
	}, 1)
	require.NoError(t, err)

	server := &swapClientServer{
		depositManager:       newTestDepositManager(),
		staticAddressManager: addrMgr,
	}

	_, err = server.GetStaticAddressSummary(
		context.Background(), &looprpc.StaticAddressSummaryRequest{},
	)
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Equal(
		t, address.ErrNoStaticAddress.Error(),
		status.Convert(err).Message(),
	)
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
	addresses, err := addrMgr.GetAllAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, addresses, 1)
	locked.AddressParams = addresses[0]

	server := &swapClientServer{
		depositManager:       newTestDepositManager(locked),
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}

	_, err = server.GetLoopInQuote(context.Background(), &looprpc.QuoteRequest{
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
	addresses, err := addrMgr.GetAllAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, addresses, 1)
	expiring.AddressParams = addresses[0]
	server := &swapClientServer{
		depositManager:       newTestDepositManager(expiring),
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}

	_, err = server.GetLoopInQuote(t.Context(), &looprpc.QuoteRequest{
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
	addresses, err := addrMgr.GetAllAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, addresses, 1)
	fresh.AddressParams = addresses[0]
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
