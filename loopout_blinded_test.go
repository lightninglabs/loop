package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/sweepbatcher"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type blindedServerMock struct {
	*serverMock

	secret  *[32]byte
	reports chan blindedRoutingReport
}

type blindedRoutingReport struct {
	hash        lntypes.Hash
	paymentAddr [32]byte
	plugin      RoutingPluginType
	success     bool
}

func (s *blindedServerMock) NewLoopOutSwap(ctx context.Context,
	hash lntypes.Hash, amount btcutil.Amount, expiry int32, key [33]byte,
	deadline time.Time, initiator string) (*newLoopOutResponse, error) {

	resp, err := s.serverMock.NewLoopOutSwap(
		ctx, hash, amount, expiry, key, deadline, initiator,
	)
	if err != nil {
		return nil, err
	}
	resp.swapInvoice, err = test.CreateBlindedInvoice(
		s.lnd.ChainParams, hash, s.swapInvoiceAmt,
		zpay32.Description(swapInvoiceDesc),
	)
	if err != nil {
		return nil, err
	}
	resp.prepayInvoice, err = test.CreateBlindedInvoice(
		s.lnd.ChainParams, s.prepayHash, s.prepayInvoiceAmt,
		zpay32.Description(prepayInvoiceDesc),
	)
	resp.paymentAddr = s.secret
	return resp, err
}

func (s *blindedServerMock) ReportRoutingResult(ctx context.Context,
	hash lntypes.Hash, paymentAddr [32]byte, plugin RoutingPluginType,
	success bool, _ int32, _ int64) error {

	if s.reports == nil {
		return nil
	}
	select {
	case s.reports <- blindedRoutingReport{hash, paymentAddr, plugin, success}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestBlindedLoopOutInitResume verifies that a blinded swap initializes and
// resumes using its explicit authentication secret, and rejects a missing one.
func TestBlindedLoopOutInitResume(t *testing.T) {
	for _, supplied := range []bool{true, false} {
		t.Run(fmt.Sprint(supplied),
			func(t *testing.T) {
				lnd := test.NewMockLnd()
				store := loopdb.NewStoreMock(t)
				server := &blindedServerMock{serverMock: newServerMock(lnd)}
				if supplied {
					server.secret = &[32]byte{1, 2, 3}
				}
				cfg := &swapConfig{
					lnd: &lnd.LndServices, store: store, server: server,
					clock: clock.NewTestClock(testTime),
				}
				result, err := newLoopOutSwap(
					t.Context(), cfg, 600, testRequest,
				)
				if !supplied {
					require.ErrorContains(t, err, "missing blinded swap")
					return
				}
				require.NoError(t, err)
				store.AssertLoopOutStored()
				resumed, err := resumeLoopOutSwap(cfg, &loopdb.LoopOut{
					Contract: &result.swap.LoopOutContract,
				})
				require.NoError(t, err)
				require.Equal(
					t, *server.secret, resumed.swapInvoicePaymentAddr,
				)
			})
	}
}

type paymentAddressRPC struct {
	swapserverrpc.SwapServerClient

	response *swapserverrpc.ServerLoopOutResponse
	request  *swapserverrpc.ServerLoopOutRequest
}

func (s *paymentAddressRPC) NewLoopOutSwap(_ context.Context,
	req *swapserverrpc.ServerLoopOutRequest,
	_ ...grpc.CallOption) (*swapserverrpc.ServerLoopOutResponse, error) {

	s.request = req
	return s.response, nil
}

// TestLoopOutResponsePaymentAddress verifies response boundary validation and
// explicit advertisement of blinded invoice support.
func TestLoopOutResponsePaymentAddress(t *testing.T) {
	for _, size := range []int{0, 31, 32, 33} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			_, pub := test.CreateKey(3)
			rpc := &paymentAddressRPC{
				response: &swapserverrpc.ServerLoopOutResponse{
					SenderKey:      pub.SerializeCompressed(),
					PaymentAddress: make([]byte, size),
				},
			}
			client := &grpcSwapServerClient{server: rpc}
			resp, err := client.NewLoopOutSwap(
				t.Context(), lntypes.Hash{1}, 100, 500,
				[33]byte{}, time.Now(), "test",
			)
			require.True(t, rpc.request.AcceptBlindedInvoices)
			if size != 0 && size != 32 {
				require.ErrorContains(t, err, "payment address length")
				return
			}
			require.NoError(t, err)
			if size == 0 {
				require.Nil(t, resp.paymentAddr)
			} else {
				require.Equal(t, &[32]byte{}, resp.paymentAddr)
			}
		})
	}
}

// TestBlindedLoopOutContractValidation verifies that blinding cannot bypass
// the swap hash, swap fee or prepayment limits checked before accepting a swap.
func TestBlindedLoopOutContractValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hash      lntypes.Hash
		swapAmt   btcutil.Amount
		prepayAmt btcutil.Amount
		wantError string
	}{
		{
			name: "valid", hash: lntypes.Hash{1},
			swapAmt: 50950, prepayAmt: 100,
		},
		{
			name: "wrong hash", hash: lntypes.Hash{2},
			swapAmt: 50950, prepayAmt: 100,
			wantError: "not equal generated swap hash",
		},
		{
			name: "excessive swap fee", hash: lntypes.Hash{1},
			swapAmt: 50951, prepayAmt: 100,
			wantError: ErrSwapFeeTooHigh.Error(),
		},
		{
			name: "excessive prepayment", hash: lntypes.Hash{1},
			swapAmt: 50949, prepayAmt: 101,
			wantError: ErrPrepayAmountTooHigh.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lnd := test.NewMockLnd()
			swapInvoice, err := test.CreateBlindedInvoice(
				lnd.ChainParams, tc.hash, tc.swapAmt,
			)
			require.NoError(t, err)
			prepayInvoice, err := test.CreateBlindedInvoice(
				lnd.ChainParams, lntypes.Hash{3}, tc.prepayAmt,
			)
			require.NoError(t, err)
			err = validateLoopOutContract(
				&lnd.LndServices, testRequest, lntypes.Hash{1},
				&newLoopOutResponse{
					swapInvoice:   swapInvoice,
					prepayInvoice: prepayInvoice,
				},
			)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestBlindedLoopOutRestart verifies that both swap resumption and cooperative
// sweep fetching recover the secret from the database after closing it.
func TestBlindedLoopOutRestart(t *testing.T) {
	lnd := test.NewMockLnd()
	dbCfg := &loopdb.SqliteConfig{
		DatabaseFileName: filepath.Join(t.TempDir(), "swaps.db"),
	}
	db, err := loopdb.NewSqliteStore(dbCfg, lnd.ChainParams)
	require.NoError(t, err)
	server := &blindedServerMock{
		serverMock: newServerMock(lnd), secret: &[32]byte{1, 2, 3},
	}
	cfg := &swapConfig{
		lnd: &lnd.LndServices, store: db, server: server,
		clock: clock.NewTestClock(testTime),
	}
	result, err := newLoopOutSwap(t.Context(), cfg, 600, testRequest)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = loopdb.NewSqliteStore(dbCfg, lnd.ChainParams)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	cfg.store = db
	stored, err := db.FetchLoopOutSwap(t.Context(), result.swap.hash)
	require.NoError(t, err)
	resumed, err := resumeLoopOutSwap(cfg, stored)
	require.NoError(t, err)
	require.Equal(t, *server.secret, resumed.swapInvoicePaymentAddr)
	fetcher, err := sweepbatcher.NewSweepFetcherFromSwapStore(
		db, lnd.ChainParams,
	)
	require.NoError(t, err)
	sweep, err := fetcher.FetchSweep(
		t.Context(), result.swap.hash, wire.OutPoint{},
	)
	require.NoError(t, err)
	require.Equal(t, *server.secret, sweep.SwapInvoicePaymentAddr)
}

// TestBlindedPaymentDelegatesToLnd verifies that blinded invoices bypass the
// destination-based routing plugin and preserve the caller's payment limits.
func TestBlindedPaymentDelegatesToLnd(t *testing.T) {
	lnd := test.NewMockLnd()
	server := &blindedServerMock{
		serverMock: newServerMock(lnd), secret: &[32]byte{1},
		reports: make(chan blindedRoutingReport, 1),
	}
	cfg := &swapConfig{
		lnd: &lnd.LndServices, store: loopdb.NewStoreMock(t), server: server,
		clock: clock.NewTestClock(testTime),
	}
	result, err := newLoopOutSwap(t.Context(), cfg, 600, testRequest)
	require.NoError(t, err)
	swap := result.swap
	swap.executeConfig = executeConfig{
		loopOutMaxParts: 5, totalPaymentTimeout: time.Minute,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := swap.payInvoiceAsync(
			ctx, swap.SwapInvoice, 100, loopdb.ChannelSet{7},
			time.Minute, RoutingPluginLowHigh, true, nil,
		)
		done <- err
	}()
	select {
	case payment := <-lnd.RouterSendPaymentChannel:
		require.Equal(t, swap.SwapInvoice, payment.Invoice)
		require.EqualValues(t, 100, payment.MaxFee)
		require.Equal(t, []uint64{7}, payment.OutgoingChanIds)
		require.EqualValues(t, 5, payment.MaxParts)
		require.Equal(t, time.Minute, payment.Timeout)
		select {
		case payment.Updates <- lndclient.PaymentStatus{
			State: lnrpc.Payment_SUCCEEDED,
		}:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	case err := <-done:
		t.Fatalf("payment was not delegated: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case report := <-server.reports:
		require.Equal(t, swap.hash, report.hash)
		require.Equal(t, *server.secret, report.paymentAddr)
		require.Equal(t, RoutingPluginNone, report.plugin)
		require.True(t, report.success)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
