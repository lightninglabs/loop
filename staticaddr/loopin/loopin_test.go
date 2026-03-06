package loopin

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// noopSigner is a minimal SignerClient mock that returns dummy signatures
// without blocking on channels.
type noopSigner struct {
	lndclient.SignerClient
}

// SignOutputRaw returns dummy 64-byte signatures for each sign descriptor.
func (s *noopSigner) SignOutputRaw(_ context.Context, _ *wire.MsgTx,
	descs []*lndclient.SignDescriptor, _ []*wire.TxOut) ([][]byte, error) {

	sigs := make([][]byte, len(descs))
	for i := range descs {
		sigs[i] = make([]byte, 64)
	}

	return sigs, nil
}

// TestCreateHtlcSweepTxUsesCorrectOutputIndex verifies that createHtlcSweepTx
// uses TxOut[htlcInputIndex].Value instead of TxOut[0].Value. When a change
// output is present, the sweep must reference the HTLC output value, not the
// change amount. It tests both output orderings to cover the regression where
// the sweep incorrectly read TxOut[0] unconditionally.
func TestCreateHtlcSweepTxUsesCorrectOutputIndex(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	network := &chaincfg.RegressionNetParams
	swapHash := lntypes.Hash{1, 2, 3}

	// Create a static address to derive PkScript.
	staticAddr, err := newStaticAddress(
		clientKey.PubKey(), serverKey.PubKey(), 4032,
	)
	require.NoError(t, err)

	pkScript, err := staticAddr.StaticAddressScript()
	require.NoError(t, err)

	addrParams := &address.Parameters{
		ClientPubkey:    clientKey.PubKey(),
		ServerPubkey:    serverKey.PubKey(),
		PkScript:        pkScript,
		Expiry:          4032,
		ProtocolVersion: version.ProtocolVersion_V0,
	}

	depositValue := btcutil.Amount(500_000)
	deposits := []*deposit.Deposit{
		{
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{0xaa},
				Index: 0,
			},
			Value: depositValue,
		},
	}

	feeRate := chainfee.SatPerKWeight(253)
	maxFeePercentage := 0.2

	// SelectedAmount < total triggers a change output.
	selectedAmount := btcutil.Amount(300_000)

	loopIn := &StaticAddressLoopIn{
		SwapHash:              swapHash,
		HtlcCltvExpiry:        800,
		InitiationHeight:      100,
		InitiationTime:        time.Now(),
		ProtocolVersion:       version.ProtocolVersion_V0,
		ClientPubkey:          clientKey.PubKey(),
		ServerPubkey:          serverKey.PubKey(),
		Deposits:              deposits,
		AddressParams:         addrParams,
		HtlcTxFeeRate:         feeRate,
		SelectedAmount:        selectedAmount,
		PaymentTimeoutSeconds: 3600,
	}

	sweepAddr, err := btcutil.NewAddressTaproot(
		make([]byte, 32), network,
	)
	require.NoError(t, err)

	signer := &noopSigner{}

	// Build the HTLC transaction once. It has two outputs with distinct
	// values: the HTLC output and a change output.
	htlcTx, err := loopIn.createHtlcTx(network, feeRate, maxFeePercentage)
	require.NoError(t, err)
	require.Len(t, htlcTx.TxOut, 2, "expected HTLC + change outputs")

	// Identify which output is change and which is HTLC.
	var htlcIdx int
	if bytes.Equal(htlcTx.TxOut[0].PkScript, pkScript) {
		htlcIdx = 1
	}

	htlcValue := htlcTx.TxOut[htlcIdx].Value
	changeValue := htlcTx.TxOut[1-htlcIdx].Value
	require.NotEqual(t, htlcValue, changeValue,
		"HTLC and change values must differ for this test to be "+
			"meaningful")

	// assertSweepUsesHtlcValue calls createHtlcSweepTx and verifies that
	// the sweep output is derived from the HTLC value, not the change.
	assertSweepUsesHtlcValue := func(t *testing.T) {
		sweepTx, err := loopIn.createHtlcSweepTx(
			context.Background(), signer, sweepAddr, feeRate,
			network, uint32(loopIn.HtlcCltvExpiry)+1,
			maxFeePercentage,
		)
		require.NoError(t, err)
		require.Len(t, sweepTx.TxOut, 1)

		sweepValue := sweepTx.TxOut[0].Value
		require.Greater(t, sweepValue, int64(0))
		require.LessOrEqual(t, sweepValue, htlcValue,
			"sweep value must not exceed HTLC output value")
		require.Greater(t, sweepValue, changeValue,
			"sweep value should be greater than change "+
				"value, confirming it was derived from "+
				"the HTLC output")
	}

	t.Run("natural output order", func(t *testing.T) {
		assertSweepUsesHtlcValue(t)
	})

	// Swap the outputs so the change is at index 0 and the HTLC is at
	// index 1. This covers the regression where createHtlcSweepTx
	// unconditionally read TxOut[0].Value.
	t.Run("swapped output order", func(t *testing.T) {
		htlcTx.TxOut[0], htlcTx.TxOut[1] = htlcTx.TxOut[1],
			htlcTx.TxOut[0]

		assertSweepUsesHtlcValue(t)
	})
}

// newStaticAddress creates a StaticAddress for testing.
func newStaticAddress(clientKey, serverKey *btcec.PublicKey,
	csvExpiry int64) (*script.StaticAddress, error) {

	return script.NewStaticAddress(
		input.MuSig2Version100RC2, csvExpiry, clientKey, serverKey,
	)
}
