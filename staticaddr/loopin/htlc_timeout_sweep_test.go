package loopin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	testStandardHtlcFeeRate = chainfee.SatPerKWeight(200)
	testHighHtlcFeeRate     = chainfee.SatPerKWeight(1_000)
	testExtremeHtlcFeeRate  = chainfee.SatPerKWeight(4_000)
	testMaxHtlcFeePct       = 0.2
	testMaxHtlcBackupFeePct = 0.5
)

// recordingSweepSigner records the sign descriptors of the htlc timeout sweep
// and returns dummy signatures.
type recordingSweepSigner struct {
	lndclient.SignerClient

	descs chan *lndclient.SignDescriptor
}

// SignOutputRaw records the sign descriptors and returns dummy 64-byte
// signatures.
func (s *recordingSweepSigner) SignOutputRaw(_ context.Context, _ *wire.MsgTx,
	descs []*lndclient.SignDescriptor, _ []*wire.TxOut) ([][]byte, error) {

	sigs := make([][]byte, len(descs))
	for i, desc := range descs {
		sigs[i] = make([]byte, 64)

		select {
		case s.descs <- desc:
		default:
		}
	}

	return sigs, nil
}

// newHtlcVariantTestLoopIn returns a loop-in over two deposits with a change
// output and the three htlc tx variants the client co-signs for it.
func newHtlcVariantTestLoopIn(t *testing.T,
	mockLnd *test.LndMockServices) (*StaticAddressLoopIn,
	map[chainfee.SatPerKWeight]*wire.MsgTx) {

	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	staticAddr, err := newStaticAddress(
		clientKey.PubKey(), serverKey.PubKey(), 4032,
	)
	require.NoError(t, err)

	pkScript, err := staticAddr.StaticAddressScript()
	require.NoError(t, err)

	deposits := []*deposit.Deposit{
		{
			Hash:  chainhash.Hash{0x11},
			Index: 1,
			Value: btcutil.Amount(600_000),
		},
		{
			Hash:  chainhash.Hash{0x22},
			Index: 0,
			Value: btcutil.Amount(400_000),
		},
	}

	sweepAddr, err := btcutil.NewAddressTaproot(
		make([]byte, 32), mockLnd.ChainParams,
	)
	require.NoError(t, err)

	loopIn := &StaticAddressLoopIn{
		SwapHash:         lntypes.Hash{7, 7, 7},
		HtlcCltvExpiry:   mockLnd.Height + 10,
		InitiationHeight: uint32(mockLnd.Height),
		InitiationTime:   time.Now(),
		ProtocolVersion:  version.ProtocolVersion_V0,
		ClientPubkey:     clientKey.PubKey(),
		ServerPubkey:     serverKey.PubKey(),
		Deposits:         deposits,
		SelectedAmount:   btcutil.Amount(800_000),
		AddressParams: &script.Parameters{
			ClientPubkey:    clientKey.PubKey(),
			ServerPubkey:    serverKey.PubKey(),
			PkScript:        pkScript,
			Expiry:          4032,
			ProtocolVersion: version.ProtocolVersion_V0,
		},
		HtlcTxFeeRate:              testStandardHtlcFeeRate,
		HtlcTxHighFeeRate:          testHighHtlcFeeRate,
		HtlcTxExtremelyHighFeeRate: testExtremeHtlcFeeRate,
		HtlcTimeoutSweepAddress:    sweepAddr,
		PaymentTimeoutSeconds:      3_600,
	}

	variants := make(map[chainfee.SatPerKWeight]*wire.MsgTx, 3)
	for _, rate := range []chainfee.SatPerKWeight{
		testStandardHtlcFeeRate, testHighHtlcFeeRate,
		testExtremeHtlcFeeRate,
	} {
		maxFeePct := testMaxHtlcBackupFeePct
		if rate == testStandardHtlcFeeRate {
			maxFeePct = testMaxHtlcFeePct
		}

		htlcTx, err := loopIn.createHtlcTx(
			mockLnd.ChainParams, rate, maxFeePct,
		)
		require.NoError(t, err)

		variants[rate] = htlcTx
	}

	return loopIn, variants
}

// TestHtlcTimeoutSweepSpendsPublishedVariant verifies that the htlc timeout
// sweep spends the htlc output of the htlc tx variant that spent the deposits
// on chain. The client co-signs a standard, a high fee and an extremely high
// fee variant and the server may publish any of them. Only the standard fee
// rate is persisted, so the sweep must not depend on the backup fee rates
// either.
func TestHtlcTimeoutSweepSpendsPublishedVariant(t *testing.T) {
	tests := []struct {
		name string
		rate chainfee.SatPerKWeight
	}{
		{
			name: "standard fee variant",
			rate: testStandardHtlcFeeRate,
		},
		{
			name: "high fee variant",
			rate: testHighHtlcFeeRate,
		},
		{
			name: "extremely high fee variant",
			rate: testExtremeHtlcFeeRate,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testHtlcTimeoutSweepSpendsVariant(t, tc.rate)
		})
	}
}

// testHtlcTimeoutSweepSpendsVariant publishes the htlc timeout sweep of a
// recovered loop-in after the htlc tx variant with the given fee rate spent
// the deposits.
func testHtlcTimeoutSweepSpendsVariant(t *testing.T,
	publishedRate chainfee.SatPerKWeight) {

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	mockLnd := test.NewMockLnd()
	loopIn, variants := newHtlcVariantTestLoopIn(t, mockLnd)

	// The three variants are distinct transactions.
	require.NotEqual(
		t, variants[testStandardHtlcFeeRate].TxHash(),
		variants[testHighHtlcFeeRate].TxHash(),
	)
	require.NotEqual(
		t, variants[testHighHtlcFeeRate].TxHash(),
		variants[testExtremeHtlcFeeRate].TxHash(),
	)

	publishedTx := variants[publishedRate]

	// Simulate a restart. The backup fee rates are not persisted, so a
	// recovered loop-in only knows the standard fee rate.
	loopIn.HtlcTxHighFeeRate = 0
	loopIn.HtlcTxExtremelyHighFeeRate = 0

	signer := &recordingSweepSigner{
		descs: make(chan *lndclient.SignDescriptor, 1),
	}

	f := newHtlcSweepTestFSM(t, ctx, mockLnd, loopIn, signer)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.SweepHtlcTimeoutAction(ctx, nil)
	}()

	// The sweep looks up the tx that spent the first deposit.
	spendReg := receiveDepositSpendReg(t, ctx, mockLnd, loopIn)
	require.Equal(t, loopIn.AddressParams.PkScript, spendReg.PkScript)
	require.Equal(t, int32(loopIn.InitiationHeight), spendReg.HeightHint)

	deliverDepositSpend(t, ctx, spendReg, publishedTx)

	var sweepTx *wire.MsgTx
	select {
	case sweepTx = <-mockLnd.TxPublishChannel:
	case <-ctx.Done():
		t.Fatalf("htlc timeout sweep not published: %v", ctx.Err())
	}

	select {
	case event := <-resultChan:
		require.Equal(t, OnHtlcTimeoutSweepPublished, event)

	case <-ctx.Done():
		t.Fatalf("sweep action did not finish: %v", ctx.Err())
	}

	htlc, err := loopIn.getHtlc(mockLnd.ChainParams)
	require.NoError(t, err)

	// The htlc output is at index 0 and the change output at index 1.
	require.Len(t, publishedTx.TxOut, 2)
	require.Equal(t, htlc.PkScript, publishedTx.TxOut[0].PkScript)

	require.Len(t, sweepTx.TxIn, 1)
	require.Equal(t, wire.OutPoint{
		Hash:  publishedTx.TxHash(),
		Index: 0,
	}, sweepTx.TxIn[0].PreviousOutPoint)

	sweepHash := sweepTx.TxHash()
	require.Equal(t, &sweepHash, loopIn.HtlcTimeoutSweepTxHash)

	// The sweep is signed for the value of the published htlc output.
	select {
	case desc := <-signer.descs:
		require.Equal(t, publishedTx.TxOut[0].Value, desc.Output.Value)
		require.Equal(t, htlc.PkScript, desc.Output.PkScript)

	default:
		t.Fatalf("htlc timeout sweep was not signed")
	}
	require.Less(t, sweepTx.TxOut[0].Value, publishedTx.TxOut[0].Value)
}

// TestHtlcOutput verifies that only a tx that spends exactly the swap's
// deposits and pays the htlc once is accepted as htlc tx.
func TestHtlcOutput(t *testing.T) {
	t.Parallel()

	mockLnd := test.NewMockLnd()
	loopIn, variants := newHtlcVariantTestLoopIn(t, mockLnd)

	htlc, err := loopIn.getHtlc(mockLnd.ChainParams)
	require.NoError(t, err)

	tests := []struct {
		name      string
		modify    func(tx *wire.MsgTx) *wire.MsgTx
		htlcIndex uint32
		err       string
	}{
		{
			name: "published variant",
			modify: func(tx *wire.MsgTx) *wire.MsgTx {
				return tx
			},
		},
		{
			name: "deposits in a different order",
			modify: func(tx *wire.MsgTx) *wire.MsgTx {
				tx.TxIn[0], tx.TxIn[1] = tx.TxIn[1], tx.TxIn[0]
				return tx
			},
		},
		{
			name: "htlc output after the change output",
			modify: func(tx *wire.MsgTx) *wire.MsgTx {
				out := tx.TxOut
				out[0], out[1] = out[1], out[0]

				return tx
			},
			htlcIndex: 1,
		},
		{
			name: "no tx",
			modify: func(*wire.MsgTx) *wire.MsgTx {
				return nil
			},
			err: "no htlc tx provided",
		},
		{
			name: "missing deposit",
			modify: func(tx *wire.MsgTx) *wire.MsgTx {
				tx.TxIn = tx.TxIn[:1]
				return tx
			},
			err: "expected the 2 deposits",
		},
		{
			name: "same deposit twice",
			modify: func(tx *wire.MsgTx) *wire.MsgTx {
				tx.TxIn[1].PreviousOutPoint =
					tx.TxIn[0].PreviousOutPoint

				return tx
			},
			err: "not a remaining deposit",
		},
		{
			name: "foreign input",
			modify: func(tx *wire.MsgTx) *wire.MsgTx {
				tx.TxIn[1].PreviousOutPoint.Index++
				return tx
			},
			err: "not a remaining deposit",
		},
		{
			name: "no htlc output",
			modify: func(tx *wire.MsgTx) *wire.MsgTx {
				tx.TxOut = tx.TxOut[1:]
				return tx
			},
			err: "has no htlc output",
		},
		{
			name: "two htlc outputs",
			modify: func(tx *wire.MsgTx) *wire.MsgTx {
				tx.TxOut[1].PkScript = htlc.PkScript
				return tx
			},
			err: "more than one htlc output",
		},
		{
			name: "htlc output without value",
			modify: func(tx *wire.MsgTx) *wire.MsgTx {
				tx.TxOut[0].Value = 0
				return tx
			},
			err: "without value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			htlcTx := tc.modify(
				variants[testHighHtlcFeeRate].Copy(),
			)

			outpoint, txOut, err := loopIn.htlcOutput(
				htlcTx, htlc.PkScript,
			)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, wire.OutPoint{
				Hash:  htlcTx.TxHash(),
				Index: tc.htlcIndex,
			}, outpoint)
			require.Equal(t, htlcTx.TxOut[tc.htlcIndex], txOut)
		})
	}
}

// TestCreateHtlcSweepTxRejectsDust verifies that the sweep is only signed if
// the fee leaves an output at or above the dust limit.
func TestCreateHtlcSweepTxRejectsDust(t *testing.T) {
	t.Parallel()

	const feeRate = chainfee.SatPerKWeight(50_000)

	mockLnd := test.NewMockLnd()
	loopIn, variants := newHtlcVariantTestLoopIn(t, mockLnd)

	htlc, err := loopIn.getHtlc(mockLnd.ChainParams)
	require.NoError(t, err)

	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddP2TROutput()
	require.NoError(t, htlc.AddSuccessToEstimator(&weightEstimator))

	fee := int64(feeRate.FeeForWeight(weightEstimator.Weight()))
	dustLimit := int64(lnwallet.DustLimitForSize(input.P2TRSize))

	tests := []struct {
		name      string
		htlcValue int64
		err       string
	}{
		{
			name:      "output at dust limit",
			htlcValue: fee + dustLimit,
		},
		{
			name:      "output below dust limit",
			htlcValue: fee + dustLimit - 1,
			err:       "too small to pay the sweep fee",
		},
		{
			name:      "fee above htlc value",
			htlcValue: fee - 1,
			err:       "too small to pay the sweep fee",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			htlcTx := variants[testExtremeHtlcFeeRate].Copy()
			htlcTx.TxOut[0].Value = tc.htlcValue

			sweepTx, err := loopIn.createHtlcSweepTx(
				t.Context(), &noopSigner{}, htlcTx,
				loopIn.HtlcTimeoutSweepAddress, feeRate,
				mockLnd.ChainParams,
				uint32(loopIn.HtlcCltvExpiry),
			)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}

			require.NoError(t, err)
			require.Equal(
				t, dustLimit, sweepTx.TxOut[0].Value,
			)
		})
	}
}

// newHtlcSweepTestFSM returns a loop-in FSM that can publish the htlc timeout
// sweep of the given loop-in against the mock lnd.
func newHtlcSweepTestFSM(t *testing.T, ctx context.Context,
	mockLnd *test.LndMockServices, loopIn *StaticAddressLoopIn,
	signer lndclient.SignerClient) *FSM {

	t.Helper()

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: loopIn.AddressParams,
		},
		ChainNotifier:                        mockLnd.ChainNotifier,
		DepositManager:                       &noopDepositManager{},
		LndClient:                            mockLnd.Client,
		ChainParams:                          mockLnd.ChainParams,
		WalletKit:                            mockLnd.WalletKit,
		Signer:                               signer,
		MaxStaticAddrHtlcFeePercentage:       testMaxHtlcFeePct,
		MaxStaticAddrHtlcBackupFeePercentage: testMaxHtlcBackupFeePct,
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	return f
}

// receiveDepositSpendReg waits for the spend registration of the first
// deposit of the loop-in.
func receiveDepositSpendReg(t *testing.T, ctx context.Context,
	mockLnd *test.LndMockServices,
	loopIn *StaticAddressLoopIn) *test.SpendRegistration {

	t.Helper()

	select {
	case spendReg := <-mockLnd.RegisterSpendChannel:
		require.Equal(
			t, loopIn.Deposits[0].OutPoint, *spendReg.Outpoint,
		)

		return spendReg

	case <-ctx.Done():
		t.Fatalf("deposit spend registration not received: %v",
			ctx.Err())

		return nil
	}
}

// deliverDepositSpend reports the given tx as the spend of the registered
// deposit.
func deliverDepositSpend(t *testing.T, ctx context.Context,
	spendReg *test.SpendRegistration, spendingTx *wire.MsgTx) {

	t.Helper()

	select {
	case spendReg.SpendChannel <- &chainntnfs.SpendDetail{
		SpentOutPoint: spendReg.Outpoint,
		SpendingTx:    spendingTx,
	}:

	case <-ctx.Done():
		t.Fatalf("unable to deliver deposit spend: %v", ctx.Err())
	}
}

// TestHtlcTimeoutSweepFeeRateAfterLookup verifies that the sweep fee rate is
// estimated after the htlc tx was found, not before waiting for it.
func TestHtlcTimeoutSweepFeeRateAfterLookup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	const (
		staleRate = chainfee.SatPerKWeight(253)
		freshRate = chainfee.SatPerKWeight(5_000)
	)

	mockLnd := test.NewMockLnd()
	mockLnd.SetFeeEstimate(defaultConfTarget, staleRate)

	loopIn, variants := newHtlcVariantTestLoopIn(t, mockLnd)
	htlcTx := variants[testHighHtlcFeeRate]
	f := newHtlcSweepTestFSM(t, ctx, mockLnd, loopIn, &noopSigner{})

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.SweepHtlcTimeoutAction(ctx, nil)
	}()

	spendReg := receiveDepositSpendReg(t, ctx, mockLnd, loopIn)

	// Fees rise while the sweep waits for the htlc tx.
	mockLnd.SetFeeEstimate(defaultConfTarget, freshRate)
	deliverDepositSpend(t, ctx, spendReg, htlcTx)

	var sweepTx *wire.MsgTx
	select {
	case sweepTx = <-mockLnd.TxPublishChannel:
	case <-ctx.Done():
		t.Fatalf("htlc timeout sweep not published: %v", ctx.Err())
	}
	require.Equal(t, OnHtlcTimeoutSweepPublished, <-resultChan)

	htlc, err := loopIn.getHtlc(mockLnd.ChainParams)
	require.NoError(t, err)

	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddP2TROutput()
	require.NoError(t, htlc.AddSuccessToEstimator(&weightEstimator))

	fee := htlcTx.TxOut[0].Value - sweepTx.TxOut[0].Value
	require.Equal(
		t, int64(freshRate.FeeForWeight(weightEstimator.Weight())),
		fee,
	)
}

// TestFetchHtlcTxFailures verifies that the htlc tx lookup fails the sweep
// attempt instead of waiting forever or sweeping without an htlc tx.
func TestFetchHtlcTxFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		lookupTimeout time.Duration
		deliver       func(ctx context.Context, shutdown func(),
			spendReg *test.SpendRegistration) bool
		err     string
		errorIs error
	}{
		{
			name: "notifier error",
			deliver: func(ctx context.Context, _ func(),
				spendReg *test.SpendRegistration) bool {

				select {
				case spendReg.ErrChan <- errors.New("closed"):
					return true

				case <-ctx.Done():
					return false
				}
			},
			err: "closed",
		},
		{
			name: "spend without tx",
			deliver: func(ctx context.Context, _ func(),
				spendReg *test.SpendRegistration) bool {

				spend := &chainntnfs.SpendDetail{}
				select {
				case spendReg.SpendChannel <- spend:
					return true

				case <-ctx.Done():
					return false
				}
			},
			err: "has no spending tx",
		},
		{
			name:          "deposit not spent",
			lookupTimeout: 100 * time.Millisecond,
			deliver: func(context.Context, func(),
				*test.SpendRegistration) bool {

				return true
			},
			err: "was not spent within",
		},
		{
			name: "shutdown",
			deliver: func(_ context.Context, shutdown func(),
				_ *test.SpendRegistration) bool {

				shutdown()
				return true
			},
			errorIs: context.Canceled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(
				t.Context(), 10*time.Second,
			)
			defer cancel()

			mockLnd := test.NewMockLnd()
			loopIn, _ := newHtlcVariantTestLoopIn(t, mockLnd)
			f := newHtlcSweepTestFSM(
				t, ctx, mockLnd, loopIn, &noopSigner{},
			)
			f.cfg.HtlcTxLookupTimeout = tc.lookupTimeout

			lookupCtx, shutdown := context.WithCancel(ctx)
			defer shutdown()

			errChan := make(chan error, 1)
			go func() {
				_, err := f.fetchHtlcTx(lookupCtx)
				errChan <- err
			}()

			spendReg := receiveDepositSpendReg(
				t, ctx, mockLnd, loopIn,
			)
			require.True(t, tc.deliver(ctx, shutdown, spendReg))

			select {
			case err := <-errChan:
				if tc.errorIs != nil {
					require.ErrorIs(t, err, tc.errorIs)
					return
				}

				require.ErrorContains(t, err, tc.err)

			case <-ctx.Done():
				t.Fatalf("htlc tx lookup did not fail: %v",
					ctx.Err())
			}
		})
	}
}
