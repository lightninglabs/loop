package instantout

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// TestMuSig2VectorLengthValidation verifies that malformed server-controlled
// vectors are rejected before they can be indexed.
func TestMuSig2VectorLengthValidation(t *testing.T) {
	_, pubKey := btcec.PrivKeyFromBytes([]byte{1})
	instantOut := &InstantOut{
		Reservations: []*reservation.Reservation{
			{
				ClientPubkey: pubKey,
				ServerPubkey: pubKey,
				Value:        btcutil.Amount(100_000),
				Expiry:       200,
				Outpoint:     &wire.OutPoint{},
			},
		},
	}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	sessions := []*input.MuSig2SessionInfo{{}}

	require.NotPanics(t, func() {
		_, err := instantOut.signMusig2Tx(
			context.Background(), nil, tx, sessions, nil,
		)
		require.ErrorContains(t, err, "server nonces")
	})

	require.NotPanics(t, func() {
		_, err := instantOut.finalizeMusig2Transaction(
			context.Background(), nil, sessions, tx, nil,
		)
		require.ErrorContains(t, err, "server signatures")
	})
}
