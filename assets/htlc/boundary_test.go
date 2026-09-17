package htlc

import (
	"bytes"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestNewSwapKitAmountLimit checks that every accepted boundary amount can
// be encoded in the funding template.
func TestNewSwapKitAmountLimit(t *testing.T) {
	params := vectorParams(t)
	params.Amount = math.MaxInt64
	kit, err := NewSwapKit(LegacyDepositV0, params)
	require.NoError(t, err)
	packet, err := kit.CreateHtlcVpkt()
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, packet.Serialize(&encoded))

	params.Amount++
	_, err = NewSwapKit(LegacyDepositV0, params)
	require.ErrorContains(t, err, "amount exceeds maximum")
}

// TestNewSwapKitRejectsOppositeKeys ensures the two script roles cannot use
// the same x-only key under different compressed encodings.
func TestNewSwapKitRejectsOppositeKeys(t *testing.T) {
	params := vectorParams(t)
	encoded := params.SenderPubKey.SerializeCompressed()
	encoded[0] ^= 1
	opposite, err := btcec.ParsePubKey(encoded)
	require.NoError(t, err)
	require.False(t, params.SenderPubKey.IsEqual(opposite))
	params.ReceiverPubKey = opposite

	_, err = NewSwapKit(LegacyDepositV0, params)
	require.ErrorContains(t, err, "keys must differ")
}
