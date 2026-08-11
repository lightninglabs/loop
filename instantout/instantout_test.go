package instantout

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
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

type cleanupTrackingSigner struct {
	lndclient.SignerClient

	cleaned [][32]byte
}

type fixedHeightLightningClient struct {
	lndclient.LightningClient

	height uint32
	err    error
}

func (c *fixedHeightLightningClient) GetInfo(context.Context) (
	*lndclient.Info, error) {

	if c.err != nil {
		return nil, c.err
	}

	return &lndclient.Info{BlockHeight: c.height}, nil
}

func (s *cleanupTrackingSigner) MuSig2Cleanup(_ context.Context,
	sessionID [32]byte) error {

	s.cleaned = append(s.cleaned, sessionID)
	return nil
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

	sessions := []*input.MuSig2SessionInfo{{}}
	_, err := instantOut.finalizeMusig2Transaction(
		context.Background(), &invalidFinalSigSigner{},
		sessions, tx, [][]byte{{1}},
	)
	require.ErrorContains(t, err, "invalid final MuSig2 signature")
	require.Nil(t, sessions[0])
}

// TestCleanupMuSig2Sessions verifies that all allocated sessions are released
// while nil entries from partial session creation are skipped.
func TestCleanupMuSig2Sessions(t *testing.T) {
	firstID := [32]byte{1}
	secondID := [32]byte{2}
	signer := &cleanupTrackingSigner{}

	err := cleanupMuSig2Sessions(
		t.Context(), signer, []*input.MuSig2SessionInfo{
			{SessionID: firstID}, nil, {SessionID: secondID},
		},
	)
	require.NoError(t, err)
	require.Equal(t, [][32]byte{firstID, secondID}, signer.cleaned)
}

// TestPushPreimageRejectsExpiringReservation verifies that recovery takes the
// on-chain fallback before revealing the preimage when a reservation is too
// close to its server-controlled timeout.
func TestPushPreimageRejectsExpiringReservation(t *testing.T) {
	instantOutFSM := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			LndClient: &fixedHeightLightningClient{height: 100},
		},
		InstantOut: &InstantOut{
			CltvExpiry: 200,
			Reservations: []*reservation.Reservation{
				{
					ID:     reservation.ID{1},
					Expiry: 139,
				},
			},
		},
	}

	event := instantOutFSM.PushPreimageAction(
		t.Context(), &RecoverInstantOutCtx{},
	)
	require.Equal(t, OnErrorPublishHtlc, event)
	require.ErrorContains(
		t, instantOutFSM.LastActionError, "before recovery safety height",
	)
}

// TestPushPreimageRejectsExpiringHtlc verifies that recovery uses a fresh
// chain height and leaves time to confirm both fallback transactions.
func TestPushPreimageRejectsExpiringHtlc(t *testing.T) {
	instantOutFSM := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			LndClient: &fixedHeightLightningClient{height: 100},
		},
		InstantOut: &InstantOut{
			CltvExpiry: 105,
			Reservations: []*reservation.Reservation{
				{
					ID:     reservation.ID{1},
					Expiry: 200,
				},
			},
		},
	}

	event := instantOutFSM.PushPreimageAction(
		t.Context(), &RecoverInstantOutCtx{},
	)
	require.Equal(t, OnErrorPublishHtlc, event)
	require.ErrorContains(
		t, instantOutFSM.LastActionError,
		"instant out HTLC expires at height 105",
	)
}
