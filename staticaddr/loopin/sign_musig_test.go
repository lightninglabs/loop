package loopin

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestSignMusig2TxRejectsMalformedInputs verifies malformed signing inputs fail
// cleanly before any MuSig2 operation is attempted.
func TestSignMusig2TxRejectsMalformedInputs(t *testing.T) {
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	network := &chaincfg.RegressionNetParams
	staticAddr, err := newStaticAddress(
		clientKey.PubKey(), serverKey.PubKey(), 4032,
	)
	require.NoError(t, err)

	pkScript, err := staticAddr.StaticAddressScript()
	require.NoError(t, err)

	addrParams := &script.Parameters{
		ClientPubkey:    clientKey.PubKey(),
		ServerPubkey:    serverKey.PubKey(),
		PkScript:        pkScript,
		Expiry:          4032,
		ProtocolVersion: version.ProtocolVersion_V0,
	}

	dep := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0xdd},
			Index: 0,
		},
		Value:         500_000,
		AddressParams: addrParams,
	}
	loopIn := &StaticAddressLoopIn{
		SwapHash:       lntypes.Hash{4, 5, 6},
		HtlcCltvExpiry: 800,
		ClientPubkey:   clientKey.PubKey(),
		ServerPubkey:   serverKey.PubKey(),
		Deposits:       []*deposit.Deposit{dep},
		HtlcTxFeeRate:  chainfee.SatPerKWeight(253),
	}

	validSessions := []*input.MuSig2SessionInfo{{}}
	validNonces := make([][musig2.PubNonceSize]byte, 1)
	tests := []struct {
		name       string
		mutateTx   func(*wire.MsgTx)
		sessions   []*input.MuSig2SessionInfo
		nonces     [][musig2.PubNonceSize]byte
		errorMatch string
	}{
		{
			name: "transaction input count",
			mutateTx: func(tx *wire.MsgTx) {
				tx.TxIn = nil
			},
			sessions:   validSessions,
			nonces:     validNonces,
			errorMatch: "htlc tx input count",
		},
		{
			name:       "session count",
			nonces:     validNonces,
			errorMatch: "musig2 session count",
		},
		{
			name:       "server nonce count",
			sessions:   validSessions,
			errorMatch: "server nonce count",
		},
		{
			name:       "nil session",
			sessions:   []*input.MuSig2SessionInfo{nil},
			nonces:     validNonces,
			errorMatch: "missing musig2 session",
		},
		{
			name: "transaction input outpoint",
			mutateTx: func(tx *wire.MsgTx) {
				tx.TxIn[0].PreviousOutPoint.Index++
			},
			sessions:   validSessions,
			nonces:     validNonces,
			errorMatch: "tx input does not match deposits",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			htlcTx, err := loopIn.createHtlcTx(
				network, loopIn.HtlcTxFeeRate, 1,
			)
			require.NoError(t, err)
			if testCase.mutateTx != nil {
				testCase.mutateTx(htlcTx)
			}

			_, err = loopIn.signMusig2Tx(
				t.Context(), htlcTx, &noopSigner{},
				testCase.sessions, testCase.nonces,
			)
			require.ErrorContains(t, err, testCase.errorMatch)
		})
	}
}
