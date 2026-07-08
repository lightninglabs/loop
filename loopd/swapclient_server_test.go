package loopd

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swap"
	mock_lnd "github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	testnetAddr, _ = btcutil.NewAddressScriptHash(
		[]byte{123}, &chaincfg.TestNet3Params,
	)

	mainnetAddr, _ = btcutil.NewAddressScriptHash(
		[]byte{123}, &chaincfg.MainNetParams,
	)

	nodepubkeyAddr, _ = btcutil.DecodeAddress(
		mock_lnd.NewMockLnd().NodePubkey, &chaincfg.MainNetParams,
	)

	chanID1 = lnwire.NewShortChanIDFromInt(1)
	chanID2 = lnwire.NewShortChanIDFromInt(2)
	chanID3 = lnwire.NewShortChanIDFromInt(3)
	chanID4 = lnwire.NewShortChanIDFromInt(4)

	peer1 = route.Vertex{1}
	peer2 = route.Vertex{2}

	channel1 = lndclient.ChannelInfo{
		Active:        false,
		ChannelID:     chanID1.ToUint64(),
		PubKeyBytes:   peer1,
		LocalBalance:  10000,
		RemoteBalance: 0,
		Capacity:      10000,
	}

	channel2 = lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     chanID2.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  10000,
		RemoteBalance: 0,
		Capacity:      10000,
	}

	channel3 = lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     chanID3.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  10000,
		RemoteBalance: 0,
		Capacity:      10000,
	}

	channel4 = lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     chanID4.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  1000,
		RemoteBalance: 0,
		Capacity:      1000,
	}
)

// TestValidateConfTarget tests all failure and success cases for our conf
// target validation function, including the case where we replace a zero
// target with the default provided.
func TestValidateConfTarget(t *testing.T) {
	const (
		// Various input confirmation values for tests.
		zeroConf int32 = 0
		oneConf  int32 = 1
		twoConf  int32 = 2
		fiveConf int32 = 5

		// defaultConf is the default confirmation target we use for
		// all tests.
		defaultConf = 6
	)

	tests := []struct {
		name           string
		confTarget     int32
		expectedTarget int32
		expectErr      bool
	}{
		{
			name:           "zero conf, get default",
			confTarget:     zeroConf,
			expectedTarget: defaultConf,
			expectErr:      false,
		},
		{
			name:       "one conf, get error",
			confTarget: oneConf,
			expectErr:  true,
		},
		{
			name:           "two conf, ok",
			confTarget:     twoConf,
			expectedTarget: twoConf,
			expectErr:      false,
		},
		{
			name:           "five conf, ok",
			confTarget:     fiveConf,
			expectedTarget: fiveConf,
			expectErr:      false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, err := validateConfTarget(
				test.confTarget, defaultConf,
			)

			if test.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, test.expectedTarget, target)
		})
	}
}

// TestValidateLoopInRequest tests validation of loop in requests.
func TestValidateLoopInRequest(t *testing.T) {
	tests := []struct {
		name               string
		amount             int64
		numDeposits        uint32
		external           bool
		confTarget         int32
		autoSelectDeposits bool
		expectErr          bool
		expectedTarget     int32
	}{
		{
			name:           "external and htlc conf set",
			amount:         100_000,
			external:       true,
			confTarget:     1,
			expectErr:      true,
			expectedTarget: 0,
		},
		{
			name:           "external and no conf",
			amount:         100_000,
			external:       true,
			confTarget:     0,
			expectErr:      false,
			expectedTarget: 0,
		},
		{
			name:           "not external, zero conf",
			amount:         100_000,
			external:       false,
			confTarget:     0,
			expectErr:      false,
			expectedTarget: loop.DefaultHtlcConfTarget,
		},
		{
			name:           "not external, bad conf",
			amount:         100_000,
			external:       false,
			confTarget:     1,
			expectErr:      true,
			expectedTarget: 0,
		},
		{
			name:           "not external, ok conf",
			amount:         100_000,
			external:       false,
			confTarget:     5,
			expectErr:      false,
			expectedTarget: 5,
		},
		{
			name:           "not external, amount no deposit",
			amount:         100_000,
			numDeposits:    0,
			external:       false,
			expectErr:      false,
			expectedTarget: loop.DefaultHtlcConfTarget,
		},
		{
			name:        "not external, deposit no amount",
			amount:      100_000,
			numDeposits: 1,
			external:    false,
			expectErr:   false,
		},

		{
			name:        "not external, deposit fractional amount",
			amount:      100_000,
			numDeposits: 1,
			external:    false,
			expectErr:   false,
		},
		{
			name:               "amount with deposit coin select",
			amount:             100_000,
			autoSelectDeposits: true,
			external:           false,
			expectErr:          false,
		},
		{
			name:               "amount with deposit coin select",
			numDeposits:        1,
			autoSelectDeposits: true,
			external:           false,
			expectErr:          true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			external := test.external
			conf, err := validateLoopInRequest(
				test.confTarget, external, test.numDeposits,
				btcutil.Amount(test.amount),
				test.autoSelectDeposits,
			)

			if test.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, test.expectedTarget, conf)
		})
	}
}

// TestStaticAddressLoopInRejectsReservedLabel verifies that external static
// loop-in requests still reject reserved autoloop labels at the RPC boundary.
func TestStaticAddressLoopInRejectsReservedLabel(t *testing.T) {
	logger := btclog.NewSLogger(
		btclog.NewDefaultHandler(os.Stdout),
	)
	setLogger(logger.SubSystem(Subsystem))

	server := &swapClientServer{}

	_, err := server.StaticAddressLoopIn(
		t.Context(), &looprpc.StaticAddressLoopInRequest{
			Label: labels.AutoloopLabel(swap.TypeIn),
		},
	)
	require.ErrorContains(t, err, labels.ErrReservedPrefix.Error())
}

// TestSetLiquidityParamsRejectsStaticAutoloopWithoutExperimental verifies that
// users must restart loopd with --experimental before enabling static-address
// autoloop.
func TestSetLiquidityParamsRejectsStaticAutoloopWithoutExperimental(
	t *testing.T) {

	server := &swapClientServer{
		config: &Config{},
	}

	_, err := server.SetLiquidityParams(
		t.Context(), &looprpc.SetLiquidityParamsRequest{
			Parameters: &looprpc.LiquidityParameters{
				LoopInSource: looprpc.
					LoopInSource_LOOP_IN_SOURCE_STATIC_ADDRESS,
			},
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "--experimental")
}

// TestStaticAddressLoopInTimestamp verifies that zero timestamps are omitted
// from static loop-in responses instead of passing a zero time to UnixNano.
func TestStaticAddressLoopInTimestamp(t *testing.T) {
	require.Zero(t, staticAddressLoopInTimestamp(time.Time{}))

	timestamp := time.Unix(1_234, 567).UTC()
	require.Equal(
		t, timestamp.UnixNano(),
		staticAddressLoopInTimestamp(timestamp),
	)
}

// TestStaticAddressLoopInSwapServerCost verifies that static loop-in server
// costs are only reported once the invoice payment was received. Timeout path
// costs are not persisted today, so they are intentionally not estimated here.
func TestStaticAddressLoopInSwapServerCost(t *testing.T) {
	const quoteFee = btcutil.Amount(1_234)

	tests := []struct {
		name       string
		state      fsm.StateType
		wantServer int64
	}{
		{
			name:  "pending before payment",
			state: loopin.SignHtlcTx,
		},
		{
			name:       "payment received",
			state:      loopin.PaymentReceived,
			wantServer: int64(quoteFee),
		},
		{
			name:       "succeeded",
			state:      loopin.Succeeded,
			wantServer: int64(quoteFee),
		},
		{
			name:       "succeeded transition failed",
			state:      loopin.SucceededTransitioningFailed,
			wantServer: int64(quoteFee),
		},
		{
			name:  "timeout swept",
			state: loopin.HtlcTimeoutSwept,
		},
		{
			name:  "failed",
			state: loopin.Failed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			swap := &loopin.StaticAddressLoopIn{
				QuotedSwapFee: quoteFee,
			}
			swap.SetState(test.state)

			costServer := staticAddressLoopInSwapServerCost(swap)

			require.Equal(t, test.wantServer, costServer)
		})
	}
}

// TestListStaticAddressSwapsPopulatesTimingAndCosts verifies that the RPC
// response maps stored static loop-in timing fields and cost fields.
func TestListStaticAddressSwapsPopulatesTimingAndCosts(t *testing.T) {
	ctx := t.Context()
	lnd := mock_lnd.NewMockLnd()

	const (
		paymentRequestAmount = btcutil.Amount(50_000)
		quotedSwapFee        = btcutil.Amount(1_234)
		depositValue         = btcutil.Amount(51_234)
		depositConfHeight    = int64(590)
		staticAddressExpiry  = uint32(25)
	)

	_, swapInvoice, err := lnd.Client.AddInvoice(
		ctx, &invoicesrpc.AddInvoiceData{
			Value: lnwire.NewMSatFromSatoshis(paymentRequestAmount),
		},
	)
	require.NoError(t, err)

	swapHash := lntypes.Hash{1, 2, 3}
	depositID := deposit.ID{4, 5, 6}
	depositOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{7, 8, 9},
		Index: 1,
	}
	testDeposit := &deposit.Deposit{
		ID:                 depositID,
		OutPoint:           depositOutpoint,
		Value:              depositValue,
		ConfirmationHeight: depositConfHeight,
		SwapHash:           &swapHash,
	}
	testDeposit.SetState(deposit.LoopedIn)

	initiationTime := time.Unix(1_234, 567).UTC()
	lastUpdateTime := time.Unix(2_345, 678).UTC()
	staticLoopIn := &loopin.StaticAddressLoopIn{
		SwapHash:         swapHash,
		SwapInvoice:      swapInvoice,
		InitiationTime:   initiationTime,
		LastUpdateTime:   lastUpdateTime,
		QuotedSwapFee:    quotedSwapFee,
		DepositOutpoints: []string{depositOutpoint.String()},
		Deposits:         []*deposit.Deposit{testDeposit},
	}
	staticLoopIn.SetState(loopin.Succeeded)

	depositStore := &mockDepositStore{
		byOutpoint: map[string]*deposit.Deposit{
			depositOutpoint.String(): testDeposit,
		},
	}
	depositMgr := deposit.NewManager(&deposit.ManagerConfig{
		Store: depositStore,
	})

	staticLoopInMgr, err := loopin.NewManager(&loopin.Config{
		Store: &mockStaticAddressLoopInStore{
			swaps: []*loopin.StaticAddressLoopIn{staticLoopIn},
		},
		DepositManager: depositMgr,
	}, 1)
	require.NoError(t, err)

	_, clientPubkey := mock_lnd.CreateKey(1)
	_, serverPubkey := mock_lnd.CreateKey(2)
	addrStore := &mockAddressStore{
		params: []*script.Parameters{{
			ClientPubkey: clientPubkey,
			ServerPubkey: serverPubkey,
			Expiry:       staticAddressExpiry,
			PkScript:     []byte("pkscript"),
		}},
	}
	addrMgr, err := address.NewManager(&address.ManagerConfig{
		Store:       addrStore,
		WalletKit:   lnd.WalletKit,
		ChainParams: lnd.ChainParams,
	}, 1)
	require.NoError(t, err)

	server := &swapClientServer{
		network:              lndclient.NetworkTestnet,
		lnd:                  &lnd.LndServices,
		staticAddressManager: addrMgr,
		depositManager:       depositMgr,
		staticLoopInManager:  staticLoopInMgr,
	}
	resp, err := server.ListStaticAddressSwaps(
		ctx, &looprpc.ListStaticAddressSwapsRequest{},
	)
	require.NoError(t, err)
	require.Len(t, resp.Swaps, 1)

	swap := resp.Swaps[0]
	require.Equal(t, swapHash[:], swap.SwapHash)
	require.Equal(t, []string{depositOutpoint.String()}, swap.DepositOutpoints)
	require.Equal(
		t, looprpc.StaticAddressLoopInSwapState_SUCCEEDED, swap.State,
	)
	require.Equal(t, int64(depositValue), swap.SwapAmountSatoshis)
	require.Equal(
		t, int64(paymentRequestAmount), swap.PaymentRequestAmountSatoshis,
	)
	require.Equal(t, initiationTime.UnixNano(), swap.InitiationTime)
	require.Equal(t, lastUpdateTime.UnixNano(), swap.LastUpdateTime)
	require.Equal(t, int64(quotedSwapFee), swap.CostServer)
	require.Zero(t, swap.CostOnchain)
	require.Zero(t, swap.CostOffchain)
	require.Len(t, swap.Deposits, 1)

	rpcDeposit := swap.Deposits[0]
	require.Equal(t, depositID[:], rpcDeposit.Id)
	require.Equal(t, depositOutpoint.String(), rpcDeposit.Outpoint)
	require.Equal(t, int64(depositValue), rpcDeposit.Value)
	require.Equal(t, depositConfHeight, rpcDeposit.ConfirmationHeight)
	require.Equal(t, swapHash[:], rpcDeposit.SwapHash)
	require.Equal(t, looprpc.DepositState_LOOPED_IN, rpcDeposit.State)
	require.Equal(
		t, depositConfHeight+int64(staticAddressExpiry)-600,
		rpcDeposit.BlocksUntilExpiry,
	)
}

// mockStaticAddressLoopInStore is a minimal in-memory loop-in store for RPC
// response mapping tests.
type mockStaticAddressLoopInStore struct {
	swaps []*loopin.StaticAddressLoopIn
}

// CreateLoopIn satisfies the static loop-in store interface.
func (s *mockStaticAddressLoopInStore) CreateLoopIn(_ context.Context,
	_ *loopin.StaticAddressLoopIn) error {

	return nil
}

// UpdateLoopIn satisfies the static loop-in store interface.
func (s *mockStaticAddressLoopInStore) UpdateLoopIn(_ context.Context,
	_ *loopin.StaticAddressLoopIn) error {

	return nil
}

// GetStaticAddressLoopInSwapsByStates returns the configured loop-ins.
func (s *mockStaticAddressLoopInStore) GetStaticAddressLoopInSwapsByStates(
	_ context.Context, _ []fsm.StateType) ([]*loopin.StaticAddressLoopIn,
	error) {

	return s.swaps, nil
}

// IsStored satisfies the static loop-in store interface.
func (s *mockStaticAddressLoopInStore) IsStored(_ context.Context,
	_ lntypes.Hash) (bool, error) {

	return false, nil
}

// GetLoopInByHash returns the configured loop-in with the given hash.
func (s *mockStaticAddressLoopInStore) GetLoopInByHash(_ context.Context,
	swapHash lntypes.Hash) (*loopin.StaticAddressLoopIn, error) {

	for _, swp := range s.swaps {
		if swp.SwapHash == swapHash {
			return swp, nil
		}
	}

	return nil, nil
}

// SwapHashesForDepositIDs satisfies the static loop-in store interface.
func (s *mockStaticAddressLoopInStore) SwapHashesForDepositIDs(
	_ context.Context, _ []deposit.ID) (map[lntypes.Hash][]deposit.ID,
	error) {

	return nil, nil
}

// TestRPCAutoloopReasonStaticLoopInNoCandidate verifies that the new planner
// reason is exposed over rpc.
func TestRPCAutoloopReasonStaticLoopInNoCandidate(t *testing.T) {
	reason, err := rpcAutoloopReason(
		liquidity.ReasonStaticLoopInNoCandidate,
	)
	require.NoError(t, err)
	require.Equal(
		t,
		looprpc.AutoReason_AUTO_REASON_STATIC_LOOP_IN_NO_CANDIDATE,
		reason,
	)
}

// TestRPCAutoloopReasonCustomChannelData verifies that custom-channel
// disqualifications are exposed over rpc instead of failing the whole dry run.
func TestRPCAutoloopReasonCustomChannelData(t *testing.T) {
	reason, err := rpcAutoloopReason(liquidity.ReasonCustomChannelData)
	require.NoError(t, err)
	require.Equal(
		t, looprpc.AutoReason_AUTO_REASON_CUSTOM_CHANNEL_DATA, reason,
	)
}

// TestSwapClientServerStopDaemon ensures that calling StopDaemon triggers the
// daemon shutdown.
func TestSwapClientServerStopDaemon(t *testing.T) {
	t.Parallel()

	// Prepare a server instance that tracks whether shutdown is requested.
	var stopCalled bool
	server := &swapClientServer{
		stopDaemon: func() {
			stopCalled = true
		},
	}

	// Request the daemon to stop and assert the callback executed.
	_, err := server.StopDaemon(
		context.Background(), &looprpc.StopDaemonRequest{},
	)
	require.NoError(t, err)

	// Ensure our shutdown callback executed.
	require.True(t, stopCalled)
}

// TestValidateLoopOutRequest tests validation of loop out requests.
func TestValidateLoopOutRequest(t *testing.T) {
	tests := []struct {
		name            string
		chain           chaincfg.Params
		confTarget      int32
		destAddr        btcutil.Address
		label           string
		channels        []lndclient.ChannelInfo
		outgoingChanSet []uint64
		amount          int64
		maxRoutingFee   int64
		maxParts        uint32
		err             error
		expectedTarget  int32
	}{
		{
			name:       "mainnet address with mainnet backend",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel2,
			},
			amount:         10000,
			maxParts:       5,
			err:            nil,
			expectedTarget: 2,
		},
		{
			name:       "mainnet address with testnet backend",
			chain:      chaincfg.TestNet3Params,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel2,
			},
			amount:         10000,
			maxParts:       5,
			err:            errIncorrectChain,
			expectedTarget: 0,
		},
		{
			name:       "testnet address with testnet backend",
			chain:      chaincfg.TestNet3Params,
			destAddr:   testnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel2,
			},
			amount:         10000,
			maxParts:       5,
			err:            nil,
			expectedTarget: 2,
		},
		{
			name:       "testnet address with mainnet backend",
			chain:      chaincfg.MainNetParams,
			destAddr:   testnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel2,
			},
			amount:         10000,
			maxParts:       5,
			err:            errIncorrectChain,
			expectedTarget: 0,
		},
		{
			name:       "invalid label",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      labels.Reserved,
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel2,
			},
			amount:         10000,
			maxParts:       5,
			err:            labels.ErrReservedPrefix,
			expectedTarget: 0,
		},
		{
			name:       "invalid conf target",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 1,
			channels: []lndclient.ChannelInfo{
				channel2,
			},
			amount:         10000,
			maxParts:       5,
			err:            errConfTargetTooLow,
			expectedTarget: 0,
		},
		{
			name:       "default conf target",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 0,
			channels: []lndclient.ChannelInfo{
				channel2,
			},
			amount:         10000,
			maxParts:       5,
			err:            nil,
			expectedTarget: 9,
		},
		{
			name:       "valid amount for default channel set",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel1, channel2, channel3,
			},
			amount:         20000,
			maxParts:       5,
			err:            nil,
			expectedTarget: 2,
		},
		{
			name:       "invalid amount for default channel set",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel1, channel2, channel3,
			},
			amount:         25000,
			maxParts:       5,
			err:            errBalanceTooLow,
			expectedTarget: 0,
		},
		{
			name:       "inactive channel in outgoing channel set",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel1, channel2, channel3,
			},
			outgoingChanSet: []uint64{
				chanID1.ToUint64(),
			},
			amount:         1000,
			maxParts:       5,
			err:            errBalanceTooLow,
			expectedTarget: 0,
		},
		{
			name:       "outgoing channel set balance is enough",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel1, channel2, channel3,
			},
			outgoingChanSet: []uint64{
				chanID2.ToUint64(),
			},
			amount:         1000,
			maxParts:       5,
			err:            nil,
			expectedTarget: 2,
		},
		{
			name:       "outgoing channel set balance not sufficient",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel1, channel2, channel3,
			},
			outgoingChanSet: []uint64{
				chanID2.ToUint64(),
			},
			amount:         20000,
			maxParts:       5,
			err:            errBalanceTooLow,
			expectedTarget: 0,
		},
		{
			name:       "amount with routing fee too high",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel2,
			},
			amount:         10000,
			maxRoutingFee:  100,
			maxParts:       5,
			err:            errBalanceTooLow,
			expectedTarget: 0,
		},
		{
			name:       "can split between channels",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel2, channel4,
			},
			amount:         11000,
			maxParts:       16,
			err:            nil,
			expectedTarget: 2,
		},
		{
			name:       "can't split between channels",
			chain:      chaincfg.MainNetParams,
			destAddr:   mainnetAddr,
			label:      "label ok",
			confTarget: 2,
			channels: []lndclient.ChannelInfo{
				channel2, channel4,
			},
			amount:         11000,
			maxParts:       5,
			err:            errBalanceTooLow,
			expectedTarget: 0,
		},
		{
			name:           "node pubkey as dest addr",
			chain:          chaincfg.MainNetParams,
			destAddr:       nodepubkeyAddr,
			err:            errInvalidAddress,
			expectedTarget: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			lnd := mock_lnd.NewMockLnd()
			lnd.Channels = test.channels

			req := &looprpc.LoopOutRequest{
				Amt:               test.amount,
				MaxSwapRoutingFee: test.maxRoutingFee,
				OutgoingChanSet:   test.outgoingChanSet,
				Label:             test.label,
				SweepConfTarget:   test.confTarget,
			}

			logger := btclog.NewSLogger(
				btclog.NewDefaultHandler(os.Stdout),
			)
			setLogger(logger.SubSystem(Subsystem))

			conf, err := validateLoopOutRequest(
				ctx, lnd.Client, &test.chain, req,
				test.destAddr, test.maxParts,
			)
			require.ErrorIs(t, err, test.err)
			require.Equal(t, test.expectedTarget, conf)
		})
	}
}

// TestHasBandwidth tests that the hasBandwidth function correctly simulates
// the MPP logic used by LND.
func TestHasBandwidth(t *testing.T) {
	tests := []struct {
		name           string
		channels       []lndclient.ChannelInfo
		maxParts       int
		amt            btcutil.Amount
		expectedRes    bool
		expectedShards int
	}{
		{
			name: "can route due to high number of parts",
			channels: []lndclient.ChannelInfo{
				{
					LocalBalance: 100,
				},
				{
					LocalBalance: 10,
				},
			},
			maxParts:       11,
			amt:            110,
			expectedRes:    true,
			expectedShards: 8,
		},
		{
			name: "can't route due to low number of parts",
			channels: []lndclient.ChannelInfo{
				{
					LocalBalance: 100,
				},
				{
					LocalBalance: 10,
				},
			},
			maxParts:    5,
			amt:         110,
			expectedRes: false,
		},
		{
			name: "can route",
			channels: []lndclient.ChannelInfo{
				{
					LocalBalance: 1000,
				},
				{
					LocalBalance: 1000,
				},
			},
			maxParts:       5,
			amt:            2000,
			expectedRes:    true,
			expectedShards: 2,
		},
		{
			name: "can route",
			channels: []lndclient.ChannelInfo{
				{
					LocalBalance: 100,
				},
				{
					LocalBalance: 100,
				},
				{
					LocalBalance: 100,
				},
			},
			maxParts:       10,
			amt:            300,
			expectedRes:    true,
			expectedShards: 10,
		},
		{
			name:           "can't route due to empty channel set",
			maxParts:       10,
			amt:            300,
			expectedRes:    false,
			expectedShards: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			res, shards := hasBandwidth(test.channels, test.amt,
				test.maxParts)
			require.Equal(t, test.expectedRes, res)
			require.Equal(t, test.expectedShards, shards)
		})
	}
}

// TestListSwapsFilterAndPagination tests the filtering and
// paging of the ListSwaps command.
func TestListSwapsFilterAndPagination(t *testing.T) {
	unixTime := time.Unix(0, 0)
	firstSwapStartTime := unixTime.Add(10 * time.Minute)
	secondSwapStartTime := unixTime.Add(20 * time.Minute)
	thirdSwapStartTime := unixTime.Add(30 * time.Minute)

	// Create a set of test swaps of various types which contain the minimal
	// viable amount of info to successfully be run through marshallSwap.
	swapInOrder0 := loop.SwapInfo{
		SwapStateData: loopdb.SwapStateData{
			State: loopdb.StateInitiated,
			Cost:  loopdb.SwapCost{},
		},
		SwapContract: loopdb.SwapContract{
			InitiationTime: firstSwapStartTime,
		},
		LastUpdate:       time.Now(),
		SwapHash:         lntypes.Hash{1},
		SwapType:         swap.TypeIn,
		HtlcAddressP2WSH: testnetAddr,
		HtlcAddressP2TR:  testnetAddr,
	}

	swapOutOrder1 := loop.SwapInfo{
		SwapStateData: loopdb.SwapStateData{
			State: loopdb.StateInitiated,
			Cost:  loopdb.SwapCost{},
		},
		SwapContract: loopdb.SwapContract{
			InitiationTime: secondSwapStartTime,
		},
		LastUpdate:       time.Now(),
		SwapHash:         lntypes.Hash{2},
		SwapType:         swap.TypeOut,
		HtlcAddressP2WSH: testnetAddr,
		HtlcAddressP2TR:  testnetAddr,
	}

	swapOutOrder2 := loop.SwapInfo{
		SwapStateData: loopdb.SwapStateData{
			State: loopdb.StateInitiated,
			Cost:  loopdb.SwapCost{},
		},
		SwapContract: loopdb.SwapContract{
			InitiationTime: thirdSwapStartTime,
		},
		LastUpdate:       time.Now(),
		SwapHash:         lntypes.Hash{3},
		SwapType:         swap.TypeOut,
		HtlcAddressP2WSH: testnetAddr,
		HtlcAddressP2TR:  testnetAddr,
	}

	mockSwaps := []loop.SwapInfo{swapInOrder0, swapOutOrder1, swapOutOrder2}

	tests := []struct {
		name string
		// Define the mock swaps that will be stored in the mock client.
		mockSwaps []loop.SwapInfo
		req       *looprpc.ListSwapsRequest
		// These hashes must be in the correct return order as the response.
		expectedReturnedSwaps []lntypes.Hash
		expectedNextStartTime int64
	}{
		{
			name:      "fetch with defaults",
			mockSwaps: mockSwaps,
			req:       &looprpc.ListSwapsRequest{},
			expectedReturnedSwaps: []lntypes.Hash{
				swapInOrder0.SwapHash,
				swapOutOrder1.SwapHash,
				swapOutOrder2.SwapHash,
			},
			expectedNextStartTime: 0,
		},
		{
			name:      "fetch with swaptype=loopin filter",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				ListSwapFilter: &looprpc.ListSwapsFilter{
					SwapType: looprpc.ListSwapsFilter_LOOP_IN},
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapInOrder0.SwapHash,
			},
			expectedNextStartTime: 0,
		},
		{
			name:      "fetch with swaptype=loopout filter",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				ListSwapFilter: &looprpc.ListSwapsFilter{
					SwapType: looprpc.ListSwapsFilter_LOOP_OUT,
				},
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapOutOrder1.SwapHash,
				swapOutOrder2.SwapHash,
			},
			expectedNextStartTime: 0,
		},
		{
			name:      "fetch with limit",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				MaxSwaps: 2,
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapInOrder0.SwapHash,
				swapOutOrder1.SwapHash,
			},
			expectedNextStartTime: secondSwapStartTime.UnixNano() + 1,
		},
		{
			name:      "fetch with limit set to default",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				MaxSwaps: 0,
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapInOrder0.SwapHash,
				swapOutOrder1.SwapHash,
				swapOutOrder2.SwapHash,
			},
			expectedNextStartTime: 0,
		},
		{
			name:      "fetch with time filter #1",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				ListSwapFilter: &looprpc.ListSwapsFilter{
					StartTimestampNs: unixTime.Add(25 * time.Minute).UnixNano(),
				},
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapOutOrder2.SwapHash,
			},
			expectedNextStartTime: 0,
		},
		{
			name:      "fetch with time filter #2",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				ListSwapFilter: &looprpc.ListSwapsFilter{
					StartTimestampNs: unixTime.Add(5 * time.Minute).UnixNano(),
				},
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapInOrder0.SwapHash,
				swapOutOrder1.SwapHash,
				swapOutOrder2.SwapHash,
			},
			expectedNextStartTime: 0,
		},
		{
			name:      "fetch with swaptype=loopout filter, time filter, limit set",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				ListSwapFilter: &looprpc.ListSwapsFilter{
					SwapType:         looprpc.ListSwapsFilter_LOOP_OUT,
					StartTimestampNs: unixTime.Add(15 * time.Minute).UnixNano(),
				},
				MaxSwaps: 1,
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapOutOrder1.SwapHash,
			},
			expectedNextStartTime: secondSwapStartTime.UnixNano() + 1,
		},
		{
			name:      "fetch with time filter, limit set",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				ListSwapFilter: &looprpc.ListSwapsFilter{
					StartTimestampNs: unixTime.UnixNano(),
				},
				MaxSwaps: 2,
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapInOrder0.SwapHash,
				swapOutOrder1.SwapHash,
			},
			expectedNextStartTime: secondSwapStartTime.UnixNano() + 1,
		},
		{
			name:      "fetch with time filter, limit set 2",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				ListSwapFilter: &looprpc.ListSwapsFilter{
					StartTimestampNs: unixTime.UnixNano(),
				},
				MaxSwaps: 3,
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapInOrder0.SwapHash,
				swapOutOrder1.SwapHash,
				swapOutOrder2.SwapHash,
			},
			expectedNextStartTime: 0,
		},
		{
			name:      "fetch with time filter edge case 1",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				ListSwapFilter: &looprpc.ListSwapsFilter{
					StartTimestampNs: secondSwapStartTime.UnixNano(),
				},
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapOutOrder1.SwapHash,
				swapOutOrder2.SwapHash,
			},
			expectedNextStartTime: 0,
		},
		{
			name:      "fetch with time filter edge case 2",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				ListSwapFilter: &looprpc.ListSwapsFilter{
					StartTimestampNs: secondSwapStartTime.UnixNano() + 1,
				},
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapOutOrder2.SwapHash,
			},
			expectedNextStartTime: 0,
		},
		{
			name:      "fetch with time filter edge case 3",
			mockSwaps: mockSwaps,
			req: &looprpc.ListSwapsRequest{
				ListSwapFilter: &looprpc.ListSwapsFilter{
					StartTimestampNs: secondSwapStartTime.UnixNano() - 1,
				},
			},
			expectedReturnedSwaps: []lntypes.Hash{
				swapOutOrder1.SwapHash,
				swapOutOrder2.SwapHash,
			},
			expectedNextStartTime: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Create the swap client server with our mock client.
			server := &swapClientServer{
				swaps: make(map[lntypes.Hash]loop.SwapInfo),
			}

			// Populate the server's swap cache with our mock swaps.
			for _, swap := range test.mockSwaps {
				server.swaps[swap.SwapHash] = swap
			}

			// Call the ListSwaps method.
			resp, err := server.ListSwaps(context.Background(), test.req)
			require.NoError(t, err)

			require.Len(
				t,
				resp.Swaps,
				len(test.expectedReturnedSwaps),
				"incorrect returned count",
			)

			// Check order of returned swaps is exactly as expected.
			for idx, aswap := range resp.Swaps {
				newhash, err := lntypes.MakeHash(aswap.GetIdBytes())
				require.NoError(t, err)
				require.Equal(
					t,
					test.expectedReturnedSwaps[idx],
					newhash,
					"iteration order mismatch",
				)
			}

			require.Equal(
				t,
				test.expectedNextStartTime,
				resp.NextStartTime,
				"incorrect next start time",
			)
		})
	}
}

// mockAddressStore is a minimal in-memory store for address parameters.
type mockAddressStore struct {
	params []*script.Parameters
}

func (s *mockAddressStore) CreateStaticAddress(_ context.Context,
	p *script.Parameters) error {

	s.params = append(s.params, p)
	return nil
}

func (s *mockAddressStore) GetStaticAddress(_ context.Context, _ []byte) (
	*script.Parameters, error) {

	if len(s.params) == 0 {
		return nil, nil
	}

	return s.params[0], nil
}

func (s *mockAddressStore) GetAllStaticAddresses(_ context.Context) (
	[]*script.Parameters, error) {

	return s.params, nil
}

// mockDepositStore implements deposit.Store minimally for DepositsForOutpoints.
type mockDepositStore struct {
	byOutpoint map[string]*deposit.Deposit
}

func (s *mockDepositStore) CreateDeposit(_ context.Context,
	_ *deposit.Deposit) error {

	return nil
}

func (s *mockDepositStore) UpdateDeposit(_ context.Context,
	_ *deposit.Deposit) error {

	return nil
}

func (s *mockDepositStore) GetDeposit(_ context.Context,
	_ deposit.ID) (*deposit.Deposit, error) {

	return nil, nil
}

func (s *mockDepositStore) DepositForOutpoint(_ context.Context,
	outpoint string) (*deposit.Deposit, error) {

	if d, ok := s.byOutpoint[outpoint]; ok {
		return d, nil
	}
	return nil, deposit.ErrDepositNotFound
}

func (s *mockDepositStore) AllDeposits(_ context.Context) ([]*deposit.Deposit,
	error) {

	deposits := make([]*deposit.Deposit, 0, len(s.byOutpoint))
	for _, d := range s.byOutpoint {
		deposits = append(deposits, d)
	}

	return deposits, nil
}

// listUnspentDepositManager backs ListUnspentDeposits tests without requiring
// the full deposit manager event loop.
type listUnspentDepositManager struct {
	byOutpoint map[string]*deposit.Deposit

	ensureDepositsFreshCalls int
	onEnsureDepositsFresh    func(*listUnspentDepositManager)
}

func (m *listUnspentDepositManager) EnsureDepositsFresh(
	context.Context) error {

	m.ensureDepositsFreshCalls++
	if m.onEnsureDepositsFresh != nil {
		m.onEnsureDepositsFresh(m)
	}

	return nil
}

func (m *listUnspentDepositManager) GetActiveDepositsInState(
	state fsm.StateType) ([]*deposit.Deposit, error) {

	deposits := make([]*deposit.Deposit, 0, len(m.byOutpoint))
	for _, d := range m.byOutpoint {
		if !d.IsInState(state) {
			continue
		}

		deposits = append(deposits, d)
	}

	return deposits, nil
}

func (m *listUnspentDepositManager) DepositsForOutpoints(
	_ context.Context, outpoints []string, ignoreUnknown bool) (
	[]*deposit.Deposit, error) {

	deposits := make([]*deposit.Deposit, 0, len(outpoints))
	seen := make(map[string]struct{}, len(outpoints))
	for i, outpoint := range outpoints {
		if _, ok := seen[outpoint]; ok {
			return nil, fmt.Errorf("duplicate outpoint %s "+
				"at index %d", outpoint, i)
		}
		seen[outpoint] = struct{}{}

		d, ok := m.byOutpoint[outpoint]
		if !ok {
			if ignoreUnknown {
				continue
			}

			return nil, deposit.ErrDepositNotFound
		}

		deposits = append(deposits, d)
	}

	return deposits, nil
}

func (m *listUnspentDepositManager) GetVisibleDeposits(
	context.Context) ([]*deposit.Deposit, error) {

	return m.allDeposits(), nil
}

func (m *listUnspentDepositManager) GetAllDeposits(
	context.Context) ([]*deposit.Deposit, error) {

	return m.allDeposits(), nil
}

func (m *listUnspentDepositManager) allDeposits() []*deposit.Deposit {
	deposits := make([]*deposit.Deposit, 0, len(m.byOutpoint))
	for _, d := range m.byOutpoint {
		deposits = append(deposits, d)
	}

	return deposits
}

// TestListUnspentDeposits tests filtering behavior of ListUnspentDeposits.
func TestListUnspentDeposits(t *testing.T) {
	ctx := context.Background()
	mock := mock_lnd.NewMockLnd()

	// Prepare a single static address parameter set.
	_, client := mock_lnd.CreateKey(1)
	_, server := mock_lnd.CreateKey(2)
	pkScript := []byte("pkscript")
	addrParams := &script.Parameters{
		ClientPubkey: client,
		ServerPubkey: server,
		Expiry:       10,
		PkScript:     pkScript,
	}

	addrStore := &mockAddressStore{params: []*script.Parameters{addrParams}}

	// Build an address manager using our mock lnd and fake address store.
	addrMgr, err := address.NewManager(&address.ManagerConfig{
		Store:       addrStore,
		WalletKit:   mock.WalletKit,
		ChainParams: mock.ChainParams,
		// ChainNotifier and AddressClient are not needed for this test.
	}, 1)
	require.NoError(t, err)

	// Construct several UTXOs with different confirmation counts.
	makeUtxo := func(idx uint32, confs int64) *lnwallet.Utxo {
		return &lnwallet.Utxo{
			AddressType:   lnwallet.TaprootPubkey,
			Value:         btcutil.Amount(250_000 + int64(idx)),
			Confirmations: confs,
			PkScript:      pkScript,
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{byte(idx + 1)},
				Index: idx,
			},
		}
	}

	utxoUnknown := makeUtxo(0, 0)
	utxoDeposited := makeUtxo(1, 1)
	utxoWithdrawn := makeUtxo(2, 2)
	utxoLoopingIn := makeUtxo(3, 5)
	utxoConfirmedUnknown := makeUtxo(4, 3)

	// Helper to build the deposit manager with specific states.
	buildDepositMgr := func(
		states map[wire.OutPoint]fsm.StateType) *listUnspentDepositManager {

		depMgr := &listUnspentDepositManager{
			byOutpoint: make(map[string]*deposit.Deposit),
		}
		for op, state := range states {
			d := &deposit.Deposit{OutPoint: op}
			d.SetState(state)
			depMgr.byOutpoint[op.String()] = d
		}

		return depMgr
	}

	// Only known Deposited records are available. Unknown deposits and
	// known non-Deposited states are excluded.
	t.Run("only known Deposited included",
		func(t *testing.T) {
			mock.SetListUnspent([]*lnwallet.Utxo{
				utxoUnknown, utxoDeposited, utxoWithdrawn,
				utxoLoopingIn,
			})

			depMgr := buildDepositMgr(map[wire.OutPoint]fsm.StateType{
				utxoDeposited.OutPoint: deposit.Deposited,
				utxoWithdrawn.OutPoint: deposit.Withdrawn,
				utxoLoopingIn.OutPoint: deposit.LoopingIn,
			})

			server := &swapClientServer{
				staticAddressManager: addrMgr,
				depositManager:       depMgr,
			}

			resp, err := server.ListUnspentDeposits(
				ctx, &looprpc.ListUnspentDepositsRequest{},
			)
			require.NoError(t, err)
			require.Equal(t, 1, depMgr.ensureDepositsFreshCalls)

			// Expect the Deposited utxo only.
			require.Len(t, resp.Utxos, 1)
			got := map[string]struct{}{}
			for _, u := range resp.Utxos {
				got[u.Outpoint] = struct{}{}
				// Confirm address string is non-empty and the
				// same across utxos.
				require.NotEmpty(t, u.StaticAddress)
			}
			_, ok := got[utxoDeposited.OutPoint.String()]
			require.True(t, ok)
		})

	// Confirmation depth no longer changes availability; state does.
	t.Run("availability ignores conf depth once deposit state is known",
		func(t *testing.T) {
			mock.SetListUnspent(
				[]*lnwallet.Utxo{
					utxoUnknown, utxoDeposited,
					utxoWithdrawn, utxoLoopingIn,
				})

			depMgr := buildDepositMgr(map[wire.OutPoint]fsm.StateType{
				utxoDeposited.OutPoint: deposit.Deposited,
				utxoWithdrawn.OutPoint: deposit.Withdrawn,
				utxoLoopingIn.OutPoint: deposit.LoopingIn,
			})

			server := &swapClientServer{
				staticAddressManager: addrMgr,
				depositManager:       depMgr,
			}

			resp, err := server.ListUnspentDeposits(
				ctx, &looprpc.ListUnspentDepositsRequest{},
			)
			require.NoError(t, err)
			require.Equal(t, 1, depMgr.ensureDepositsFreshCalls)

			require.Len(t, resp.Utxos, 1)
			got := map[string]struct{}{}
			for _, u := range resp.Utxos {
				got[u.Outpoint] = struct{}{}
			}
			_, ok := got[utxoDeposited.OutPoint.String()]
			require.True(t, ok)
		})

	// A wallet-visible UTXO reconciled by EnsureDepositsFresh should be
	// returned in the same ListUnspentDeposits call.
	t.Run("freshly reconciled wallet utxo is included", func(t *testing.T) {
		mock.SetListUnspent([]*lnwallet.Utxo{utxoConfirmedUnknown})

		depMgr := buildDepositMgr(map[wire.OutPoint]fsm.StateType{})
		depMgr.onEnsureDepositsFresh = func(
			m *listUnspentDepositManager) {

			d := &deposit.Deposit{
				OutPoint: utxoConfirmedUnknown.OutPoint,
			}
			d.SetState(deposit.Deposited)
			m.byOutpoint[d.OutPoint.String()] = d
		}

		server := &swapClientServer{
			staticAddressManager: addrMgr,
			depositManager:       depMgr,
		}

		resp, err := server.ListUnspentDeposits(
			ctx, &looprpc.ListUnspentDepositsRequest{},
		)
		require.NoError(t, err)
		require.Equal(t, 1, depMgr.ensureDepositsFreshCalls)

		require.Len(t, resp.Utxos, 1)
		require.Equal(
			t, utxoConfirmedUnknown.OutPoint.String(),
			resp.Utxos[0].Outpoint,
		)
	})
}
