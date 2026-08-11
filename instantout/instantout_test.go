package instantout

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

type invalidFinalSigSigner struct {
	lndclient.SignerClient
}

func (s *invalidFinalSigSigner) MuSig2CombineSig(context.Context, [32]byte,
	[][]byte) (bool, []byte, error) {

	return true, make([]byte, 64), nil
}

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

// TestFinalizeMuSig2TransactionVerifiesSignature verifies that a combined
// signature is validated locally before the transaction can be used as the
// instant-out safety net.
func TestFinalizeMuSig2TransactionVerifiesSignature(t *testing.T) {
	_, pubKey := btcec.PrivKeyFromBytes([]byte{1})
	res := &reservation.Reservation{
		ClientPubkey: pubKey,
		ServerPubkey: pubKey,
		Value:        btcutil.Amount(100_000),
		Expiry:       200,
		Outpoint:     &wire.OutPoint{},
	}
	instantOut := &InstantOut{
		Reservations: []*reservation.Reservation{res},
	}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: *res.Outpoint})
	tx.AddTxOut(&wire.TxOut{Value: 90_000})

	_, err := instantOut.finalizeMusig2Transaction(
		context.Background(), &invalidFinalSigSigner{},
		[]*input.MuSig2SessionInfo{{}}, tx, [][]byte{{1}},
	)
	require.ErrorContains(t, err, "invalid final MuSig2 signature")
}
