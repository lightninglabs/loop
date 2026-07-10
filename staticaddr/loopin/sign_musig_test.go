package loopin

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestSignMusig2TxRejectsNonceCountMismatch verifies malformed server nonce
// sets fail cleanly instead of panicking when signing HTLC variants.
func TestSignMusig2TxRejectsNonceCountMismatch(t *testing.T) {
	t.Parallel()

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

	addrParams := &address.Parameters{
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

	htlcTx, err := loopIn.createHtlcTx(network, loopIn.HtlcTxFeeRate, 1)
	require.NoError(t, err)

	_, err = loopIn.signMusig2Tx(
		t.Context(), htlcTx, &noopSigner{},
		[]*input.MuSig2SessionInfo{{}}, nil,
	)
	require.ErrorContains(t, err, "server nonce count")
}
