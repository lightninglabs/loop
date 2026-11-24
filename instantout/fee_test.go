package instantout

import (
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// feeTxBuilder builds an instant-out transaction at the given fee rate.
type feeTxBuilder func(io *InstantOut, rate chainfee.SatPerKWeight) (
	*wire.MsgTx, error)

// feeTestInstantOut returns an instant out with one reservation holding the
// full swap value.
func feeTestInstantOut(clientKey, serverKey *btcec.PublicKey,
	value btcutil.Amount) *InstantOut {

	return &InstantOut{
		clientPubkey: clientKey,
		serverPubkey: serverKey,
		CltvExpiry:   180,
		Value:        value,
		Reservations: []*reservation.Reservation{{
			ClientPubkey: clientKey,
			ServerPubkey: serverKey,
			Expiry:       200,
			Outpoint:     &wire.OutPoint{},
			Value:        value,
		}},
	}
}

// TestInstantOutFeeCapArithmetic checks the actual builders at the integer fee
// cap, fractional rounding and the signed multiplication overflow boundary.
func TestInstantOutFeeCapArithmetic(t *testing.T) {
	_, clientKey := btcec.PrivKeyFromBytes([]byte{1})
	_, serverKey := btcec.PrivKeyFromBytes([]byte{2})
	address, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.TestNet3Params,
	)
	require.NoError(t, err)
	sweepWeight := sweeplessSweepWeight(1)
	const minRelay = chainfee.SatPerKWeight(253)

	builders := []struct {
		name   string
		weight lntypes.WeightUnit
		build  feeTxBuilder
	}{
		{
			name:   "HTLC",
			weight: htlcWeight(1),
			build: func(io *InstantOut, r chainfee.SatPerKWeight) (
				*wire.MsgTx, error) {

				io.htlcFeeRate = r

				return io.createHtlcTransaction(
					&chaincfg.TestNet3Params, minRelay,
				)
			},
		},
		{
			name:   "cooperative",
			weight: sweepWeight,
			build: func(io *InstantOut, r chainfee.SatPerKWeight) (
				*wire.MsgTx, error) {

				io.sweepAddress = address
				return io.createSweeplessSweepTx(r, minRelay)
			},
		},
	}

	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			capFee := btcutil.Amount(b.weight)
			floorFee := capFee + capFee/1000
			overflowRate := chainfee.SatPerKWeight(
				math.MaxInt64/int64(b.weight) + 1,
			)

			// Wrap past 2^64 to a small positive fee that the old
			// arithmetic accepted with a positive relay minimum.
			positiveWrapRate := chainfee.SatPerKWeight(
				math.MaxUint64/uint64(b.weight)+1,
			) + 1000
			wrappedFee := positiveWrapRate.FeeForWeight(b.weight)
			require.Positive(t, wrappedFee)
			require.Less(t, wrappedFee, btcutil.Amount(20_000))
			require.GreaterOrEqual(t,
				chainfee.NewSatPerKWeight(wrappedFee, b.weight),
				minRelay)

			cases := []struct {
				name    string
				value   btcutil.Amount
				rate    chainfee.SatPerKWeight
				wantFee btcutil.Amount
				wantErr bool
			}{
				{
					name:    "exact cap",
					value:   capFee * 5,
					rate:    1000,
					wantFee: capFee,
				},
				{
					name:    "fractional fee floor",
					value:   floorFee * 5,
					rate:    1001,
					wantFee: floorFee,
				},
				{
					name:    "one sat over cap",
					value:   capFee*5 - 1,
					rate:    1000,
					wantErr: true,
				},
				{
					name:    "product overflow",
					value:   100_000,
					rate:    overflowRate,
					wantErr: true,
				},
				{
					name:    "positive wrapped fee",
					value:   100_000,
					rate:    positiveWrapRate,
					wantErr: true,
				},
				{
					name:    "negative rate",
					value:   100_000,
					rate:    -1,
					wantErr: true,
				},
				{
					name:    "zero rate",
					value:   100_000,
					wantErr: true,
				},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					io := feeTestInstantOut(
						clientKey, serverKey, tc.value,
					)
					tx, err := b.build(io, tc.rate)
					if tc.wantErr {
						require.Error(t, err)
						require.Nil(t, tx)

						return
					}

					require.NoError(t, err)
					require.Len(t, tx.TxOut, 1)
					require.Equal(
						t, int64(tc.value-tc.wantFee),
						tx.TxOut[0].Value,
					)
				})
			}
		})
	}
}
