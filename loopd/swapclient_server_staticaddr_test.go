package loopd

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/txscript"
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
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest/port"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/assert"
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

// SetOnchainCosts implements deposit.Store for static address server tests.
func (s *staticAddrDepositStore) SetOnchainCosts(context.Context,
	map[deposit.ID]deposit.OnchainCost) error {

	return nil
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
	return newFundingAddressManager(t, expiry, &mock.LndServices), mock
}

// newFundingAddressManager uses a local root agreement and the supplied lnd
// services for receive key derivation, tapscript import and chain events.
func newFundingAddressManager(t *testing.T, expiry uint32,
	lnd *lndclient.LndServices) *address.Manager {

	t.Helper()
	// The mock derives keys from one counter across families. Keep the root
	// distinct from the receive keys at indexes zero and one used below.
	_, client := mock_lnd.CreateKey(100)
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
		WalletKit:     lnd.WalletKit,
		ChainParams:   lnd.ChainParams,
		ChainNotifier: lnd.ChainNotifier,
	}, 1)
	require.NoError(t, err)

	initChan := make(chan struct{})
	errChan := make(chan error, 1)
	go func() {
		errChan <- addrMgr.Run(t.Context(), initChan)
	}()
	select {
	case <-initChan:
	case err := <-errChan:
		t.Fatalf("start address manager: %v", err)
	case <-t.Context().Done():
		t.Fatal("address manager initialization canceled")
	}

	return addrMgr
}

// TestNewStaticAddressDoesNotFund verifies address creation never invokes the
// wallet's SendCoins method, even with the removed funding field on the wire.
func TestNewStaticAddressDoesNotFund(t *testing.T) {
	t.Parallel()

	addrMgr, _ := newTestStaticAddressContext(t, 10)
	// Leave the wallet dependency nil so any funding access fails the test.
	server := &swapClientServer{staticAddressManager: addrMgr}

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
}

type fundingLightningClient struct {
	lndclient.LightningClient

	wallet lnrpc.LightningClient
}

func (c *fundingLightningClient) RawClientWithMacAuth(ctx context.Context) (
	context.Context, time.Duration, lnrpc.LightningClient) {

	return ctx, time.Second, c.wallet
}

type fundingWallet struct {
	lnrpc.LightningClient

	request *lnrpc.SendCoinsRequest
	reserve int64
	err     error
}

func (w *fundingWallet) WalletBalance(context.Context, *lnrpc.WalletBalanceRequest,
	...grpc.CallOption) (*lnrpc.WalletBalanceResponse, error) {

	return &lnrpc.WalletBalanceResponse{ReservedBalanceAnchorChan: w.reserve}, nil
}

func (w *fundingWallet) SendCoins(_ context.Context, req *lnrpc.SendCoinsRequest,
	_ ...grpc.CallOption) (*lnrpc.SendCoinsResponse, error) {

	w.request = req
	return &lnrpc.SendCoinsResponse{Txid: "funding-txid"}, w.err
}

// TestFundStaticAddress checks that lnd sends only to the freshly derived
// address and receives the caller's wallet options.
func TestFundStaticAddress(t *testing.T) {
	t.Parallel()
	addrMgr, mock := newTestStaticAddressContext(t, 10)
	wallet := &fundingWallet{}
	server := &swapClientServer{
		staticAddressManager: addrMgr,
		lnd: &lndclient.LndServices{
			Client:    &fundingLightningClient{wallet: wallet},
			WalletKit: mock.WalletKit,
		},
	}

	op := wire.OutPoint{Hash: chainhash.Hash{0xab}, Index: 1}
	mock.SetFeeEstimate(6, 500)
	mock.SetListUnspent([]*lnwallet.Utxo{{
		OutPoint: op, Value: 200_000, AddressType: lnwallet.WitnessPubKey,
	}})
	req := &lnrpc.SendCoinsRequest{
		Amount: 100000, TargetConf: 6, SpendUnconfirmed: true,
		Outpoints:             []*lnrpc.OutPoint{{TxidBytes: op.Hash[:], OutputIndex: 1}},
		Label:                 "deposit",
		CoinSelectionStrategy: lnrpc.CoinSelectionStrategy_STRATEGY_RANDOM,
	}
	want := proto.Clone(req).(*lnrpc.SendCoinsRequest)
	resp, err := server.FundStaticAddress(t.Context(), req)
	require.NoError(t, err)
	require.True(t, proto.Equal(want, req))
	require.NotEmpty(t, resp.Address)
	require.EqualValues(t, 10, resp.Expiry)
	require.Equal(t, "funding-txid", resp.GetSendCoinsResponse().GetTxid())
	want.TargetConf = 0
	want.SatPerVbyte = 2
	want.Addr = resp.Address
	want.Outpoints[0].TxidStr = op.Hash.String()
	require.True(t, proto.Equal(want, wallet.request))

	wallet.err = status.Error(codes.Unavailable, "lost lnd connection")
	_, err = server.FundStaticAddress(t.Context(), &lnrpc.SendCoinsRequest{
		SendAll: true, SatPerVbyte: 2, MinConfs: 3,
	})
	require.ErrorContains(t, err, wallet.request.Addr)
	require.ErrorContains(t, err, "check wallet transactions")
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.NotEqual(t, resp.Address, wallet.request.Addr)
	require.Equal(t, "Loop static deposit "+wallet.request.Addr, wallet.request.Label)
	require.Len(t, wallet.request.Outpoints, 1)
	require.True(t, wallet.request.SendAll)
	require.EqualValues(t, 2, wallet.request.SatPerVbyte)
	require.EqualValues(t, 3, wallet.request.MinConfs)
}

// Invalid funding requests must fail before accessing the address manager or wallet.
func TestFundStaticAddressInvalidRequest(t *testing.T) {
	t.Parallel()
	const amount = looprpc.MinStaticDepositSat
	op := &lnrpc.OutPoint{TxidStr: strings.Repeat("01", 32)}
	for _, req := range []*lnrpc.SendCoinsRequest{
		nil, {}, {Amount: amount - 1},
		{Amount: amount, SatPerVbyte: looprpc.MaxStaticDepositSatPerVByte + 1},
		{Amount: -1},
		{Amount: amount, SendAll: true},
		{Amount: btcutil.MaxSatoshi + 1},
		{Amount: amount, Addr: "external-destination"},
		{Amount: amount, SatPerByte: 5}, //nolint:staticcheck
		{Amount: amount, Outpoints: []*lnrpc.OutPoint{nil}},
		{Amount: amount, Outpoints: []*lnrpc.OutPoint{{TxidBytes: []byte{1}}}},
		{Amount: amount, Outpoints: []*lnrpc.OutPoint{{
			TxidStr: op.TxidStr, TxidBytes: make([]byte, 32),
		}}},
		{Amount: amount, TargetConf: 3, SatPerVbyte: 1},
		{Amount: amount, TargetConf: -1},
		{Amount: amount, SatPerVbyte: ^uint64(0)},
		{Amount: amount, MinConfs: -1},
		{Amount: amount, MinConfs: 1, SpendUnconfirmed: true},
		{Amount: amount, CoinSelectionStrategy: 42},
		{Amount: amount, Outpoints: []*lnrpc.OutPoint{{TxidStr: "bad"}}},
		{Amount: amount, Outpoints: []*lnrpc.OutPoint{op, op}},
		{Amount: amount, Label: strings.Repeat("x", 501)},
	} {
		server := &swapClientServer{}
		_, err := server.FundStaticAddress(t.Context(), req)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", req)
	}
}

// fundingAccountWallet verifies the account and confirmation filters applied
// before signing, including for a sweep with no explicit inputs.
type fundingAccountWallet struct {
	lndclient.WalletKitClient

	t     *testing.T
	value btcutil.Amount
	rate  chainfee.SatPerKWeight
}

func (w *fundingAccountWallet) EstimateFeeRate(_ context.Context,
	target int32) (chainfee.SatPerKWeight, error) {

	require.EqualValues(w.t, 6, target)
	return w.rate, nil
}

func (w *fundingAccountWallet) ListUnspent(_ context.Context, minConfs,
	maxConfs int32, opts ...lndclient.ListUnspentOption) ([]*lnwallet.Utxo, error) {

	req := &walletrpc.ListUnspentRequest{}
	for _, opt := range opts {
		opt(req)
	}
	require.Equal(w.t, lnwallet.DefaultAccountName, req.Account)
	require.EqualValues(w.t, 1, minConfs)
	require.EqualValues(w.t, math.MaxInt32, maxConfs)
	return []*lnwallet.Utxo{{Value: w.value, AddressType: lnwallet.TaprootPubkey}}, nil
}

func TestStaticDepositWalletPreflight(t *testing.T) {
	t.Parallel()
	// No address manager: every failure must precede address issuance.
	wallet := &fundingWallet{}
	account := &fundingAccountWallet{t: t, value: 20_000}
	server := &swapClientServer{lnd: &lndclient.LndServices{
		WalletKit: account, Client: &fundingLightningClient{wallet: wallet},
	}}
	op := lnrpc.MarshalOutPoint(&wire.OutPoint{Index: 9})
	for _, req := range []*lnrpc.SendCoinsRequest{
		{Amount: looprpc.MinStaticDepositSat, Outpoints: []*lnrpc.OutPoint{op}},
		{SendAll: true, Outpoints: []*lnrpc.OutPoint{op}},
		{Amount: int64(account.value) - 1},
	} {
		req.SatPerVbyte = 1
		_, err := server.FundStaticAddress(t.Context(), req)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
	}
	account.rate = chainfee.SatPerVByte(looprpc.MaxStaticDepositSatPerVByte).FeePerKWeight() + 1
	_, err := server.FundStaticAddress(t.Context(), &lnrpc.SendCoinsRequest{SendAll: true})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "fee estimate")

	account.value = looprpc.MinStaticDepositSat + 10_000
	wallet.reserve = 10_000
	_, err = server.FundStaticAddress(t.Context(), &lnrpc.SendCoinsRequest{
		SendAll: true, SatPerVbyte: 2,
	})
	require.ErrorContains(t, err, "after fees and wallet reserve")
	require.Nil(t, wallet.request)
}

// TestMinimumStaticDepositWithdrawal checks the economic reason for the
// funding floor against the cooperative withdrawal transaction itself.
func TestMinimumStaticDepositWithdrawal(t *testing.T) {
	t.Parallel()
	addr, err := btcutil.NewAddressTaproot(make([]byte, 32), &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	rate := chainfee.SatPerVByte(looprpc.MaxStaticDepositSatPerVByte).FeePerKWeight()
	amount, _, err := withdraw.CalculateWithdrawalTxValues(
		[]*deposit.Deposit{{Value: looprpc.MinStaticDepositSat}}, 0, rate,
		addr, lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
	)
	require.NoError(t, err)
	require.GreaterOrEqual(t, amount, lnwallet.DustLimitForSize(input.P2TRSize))
}

func TestStaticDepositFundingFields(t *testing.T) {
	t.Parallel()
	extra := protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1)
	op := lnrpc.MarshalOutPoint(&wire.OutPoint{})
	req := &lnrpc.SendCoinsRequest{Amount: looprpc.MinStaticDepositSat,
		SatPerVbyte: looprpc.MaxStaticDepositSatPerVByte, Outpoints: []*lnrpc.OutPoint{op}}
	_, err := staticAddressFundingRequest(req)
	require.NoError(t, err)
	req.ProtoReflect().SetUnknown(extra)
	_, err = staticAddressFundingRequest(req)
	require.ErrorContains(t, err, "unsupported SendCoins")
	req.ProtoReflect().SetUnknown(nil)
	op.ProtoReflect().SetUnknown(extra)
	_, err = staticAddressFundingRequest(req)
	require.ErrorContains(t, err, "unsupported funding outpoint")
}

var (
	fundingTestLnd  = flag.String("lndexec", "", "path to lnd built with wallet RPCs")
	fundingTestBtcd = flag.String("btcdexec", "", "path to btcd for the test miner")
)

// startDepositWallet starts a fresh lnd and miner isolated from local wallets.
func startDepositWallet(t *testing.T) (*lndclient.GrpcLndServices, *rpctest.Harness) {
	t.Helper()
	miner, err := rpctest.New(&chaincfg.RegressionNetParams, nil,
		[]string{"--txindex"}, *fundingTestBtcd)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, miner.TearDown()) })
	// Activate SegWit and Taproot before funding the wallet.
	require.NoError(t, miner.SetUp(true, 432))
	dir := t.TempDir()
	rpcAddr := fmt.Sprintf("127.0.0.1:%d", port.NextAvailablePort())
	cfg := miner.RPCConfig()
	daemon := exec.Command(*fundingTestLnd, "--lnddir="+dir,
		"--rpclisten="+rpcAddr, "--noseedbackup", "--nolisten", "--norest",
		"--bitcoin.regtest", "--bitcoin.node=btcd", "--btcd.rpchost="+cfg.Host,
		"--btcd.rpcuser="+cfg.User, "--btcd.rpcpass="+cfg.Pass,
		"--btcd.rawrpccert="+hex.EncodeToString(cfg.Certificates))
	daemon.Stderr = os.Stderr
	require.NoError(t, daemon.Start())
	t.Cleanup(func() {
		_ = daemon.Process.Signal(os.Interrupt)
		_ = daemon.Wait()
	})
	var services *lndclient.GrpcLndServices
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		services, err = lndclient.NewLndServices(&lndclient.LndServicesConfig{
			LndAddress: rpcAddr, Network: lndclient.NetworkRegtest,
			TLSPath: filepath.Join(dir, "tls.cert"), BlockUntilChainSynced: true,
			MacaroonDir: filepath.Join(dir, "data", "chain", "bitcoin", "regtest"),
			CallerCtx:   t.Context(),
		})
		assert.NoError(c, err)
	}, time.Minute, 100*time.Millisecond)
	t.Cleanup(services.Close)
	return services, miner
}

// confirmDepositTx mines the expected transaction and waits for lnd to
// process the resulting block before checking its confirmed coins.
func confirmDepositTx(t *testing.T, miner *rpctest.Harness,
	services *lndclient.GrpcLndServices, hash *chainhash.Hash) {

	t.Helper()
	hashes, err := miner.Client.Generate(1)
	require.NoError(t, err)
	block, err := miner.Client.GetBlock(hashes[0])
	require.NoError(t, err)
	require.Len(t, block.Transactions, 2)
	require.Equal(t, *hash, block.Transactions[1].TxHash())
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		info, err := services.Client.GetInfo(t.Context())
		if assert.NoError(c, err) {
			assert.Equal(c, *hashes[0], info.BestBlockHash)
		}
	}, 15*time.Second, 100*time.Millisecond)
}

// TestStaticDepositFundingAccounts funds a Loop tapscript in a real lnd
// wallet, then exercises explicit coin control and an unqualified sweep.
func TestStaticDepositFundingAccounts(t *testing.T) {
	if *fundingTestLnd == "" {
		t.Skip("pass -lndexec and -btcdexec to run the regtest wallet test")
	}
	services, miner := startDepositWallet(t)
	ctx := t.Context()
	addr, err := services.WalletKit.NextAddr(ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_WITNESS_PUBKEY_HASH, false)
	require.NoError(t, err)
	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)
	funding, err := miner.SendOutputs([]*wire.TxOut{wire.NewTxOut(200_000, script)}, 2)
	require.NoError(t, err)
	confirmDepositTx(t, miner, services, funding)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		coins, err := services.WalletKit.ListUnspent(ctx, 1, 100,
			lndclient.WithUnspentAccount(lnwallet.DefaultAccountName))
		if assert.NoError(c, err) {
			assert.Len(c, coins, 1)
		}
	}, 15*time.Second, 100*time.Millisecond)
	server := &swapClientServer{lnd: &services.LndServices,
		staticAddressManager: newFundingAddressManager(t, 100, &services.LndServices)}
	first, err := server.FundStaticAddress(ctx, &lnrpc.SendCoinsRequest{
		Amount: 50_000, SatPerVbyte: 2,
	})
	require.NoError(t, err)
	firstHash, err := chainhash.NewHashFromStr(first.GetSendCoinsResponse().GetTxid())
	require.NoError(t, err)
	confirmDepositTx(t, miner, services, firstHash)

	// ChainIO can observe the block before the wallet credits the
	// imported script. Wait for the confirmed wallet view itself.
	deposits := func(count int) []wire.OutPoint {
		t.Helper()
		var result []wire.OutPoint
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			coins, err := server.staticAddressManager.ListUnspent(ctx, 1, 100)
			if !assert.NoError(c, err) || !assert.Len(c, coins, count) {
				return
			}
			result = nil
			for _, coin := range coins {
				result = append(result, coin.OutPoint)
			}
		}, 15*time.Second, 100*time.Millisecond)
		return result
	}
	deposit := deposits(1)[0]
	require.Equal(t, first.GetSendCoinsResponse().GetTxid(), deposit.Hash.String())
	addrBefore, err := server.staticAddressManager.GetAllAddresses(ctx)
	require.NoError(t, err)
	for _, req := range []*lnrpc.SendCoinsRequest{
		{Amount: looprpc.MinStaticDepositSat}, {SendAll: true},
	} {
		req.Outpoints = []*lnrpc.OutPoint{lnrpc.MarshalOutPoint(&deposit)}
		req.SatPerVbyte = 2
		_, err := server.FundStaticAddress(ctx, req)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		require.ErrorContains(t, err, "default-account input")
	}
	addrAfter, err := server.staticAddressManager.GetAllAddresses(ctx)
	require.NoError(t, err)
	require.Len(t, addrAfter, len(addrBefore))

	sweep, err := server.FundStaticAddress(ctx, &lnrpc.SendCoinsRequest{
		SendAll: true, SatPerVbyte: 2,
	})
	require.NoError(t, err)
	sweepHash, err := chainhash.NewHashFromStr(sweep.GetSendCoinsResponse().GetTxid())
	require.NoError(t, err)
	tx, err := miner.Client.GetRawTransaction(sweepHash)
	require.NoError(t, err)
	require.Len(t, tx.MsgTx().TxIn, 1)
	spent := tx.MsgTx().TxIn[0].PreviousOutPoint
	require.Equal(t, *firstHash, spent.Hash)
	require.NotEqual(t, deposit, spent)
	confirmDepositTx(t, miner, services, sweepHash)
	require.ElementsMatch(t, []wire.OutPoint{deposit, {Hash: *sweepHash}}, deposits(2))
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
		network:              lndclient.NetworkTestnet,
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
		Hash:          chainhash.Hash{3},
		Index:         3,
		AddressParams: addresses[0],
	}
	d.SetState(deposit.Withdrawn)

	server := &swapClientServer{
		network: lndclient.NetworkTestnet,
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
		Hash:  chainhash.Hash{7},
		Index: 7,
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
		Hash:               chainhash.Hash{8},
		Index:              8,
		ConfirmationHeight: confirmationHeight,
		AddressParams: &script.Parameters{
			Expiry: 20,
		},
	}
	second := &deposit.Deposit{
		Hash:               chainhash.Hash{9},
		Index:              9,
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
		Hash:               chainhash.Hash{10},
		Index:              10,
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
		network:              lndclient.NetworkTestnet,
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
		depositManager: &listUnspentDepositManager{
			byOutpoint: map[string]*deposit.Deposit{
				locked.OutPoint.String(): locked,
			},
		},
		staticAddressManager: addrMgr,
		lnd:                  &lnd.LndServices,
	}

	_, err = server.GetLoopInQuote(context.Background(), &looprpc.QuoteRequest{
		DepositOutpoints: []string{locked.OutPoint.String()},
	})
	require.ErrorContains(t, err, "is not currently available")
	require.ErrorContains(t, err, locked.OutPoint.String())
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
		depositManager: &listUnspentDepositManager{
			byOutpoint: map[string]*deposit.Deposit{
				expiring.OutPoint.String(): expiring,
			},
		},
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
	depMgr := &listUnspentDepositManager{byOutpoint: map[string]*deposit.Deposit{
		fresh.OutPoint.String(): fresh,
	}}
	addresses, err := addrMgr.GetAllAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, addresses, 1)
	fresh.AddressParams = addresses[0]
	server := &swapClientServer{
		depositManager:       depMgr,
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
	require.Equal(t, 1, depMgr.activeLookupCalls)
	require.Zero(t, depMgr.recordLookupCalls)
	require.Zero(t, depMgr.visibleLookupCalls)

	// A rejected selection names the deposit that is not available. A
	// duplicate selection only fails as a whole.
	unknown := wire.OutPoint{Hash: chainhash.Hash{99}}.String()
	duplicate := []string{fresh.OutPoint.String(), fresh.OutPoint.String()}
	for _, invalid := range []struct {
		outpoints []string
		err       string
	}{
		{outpoints: duplicate, err: "outpoints must be unique"},
		{outpoints: []string{unknown}, err: "deposit " + unknown},
		{outpoints: []string{"invalid"}, err: "deposit invalid"},
	} {
		quoter.request = nil
		_, err := server.GetLoopInQuote(t.Context(), &looprpc.QuoteRequest{
			DepositOutpoints: invalid.outpoints,
		})
		require.ErrorContains(t, err, invalid.err)
		require.Nil(t, quoter.request)
	}
}
