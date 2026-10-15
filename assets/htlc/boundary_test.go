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
	kit, err := NewSwapKit(AssetHTLCV0, params)
	require.NoError(t, err)
	packet, err := kit.CreateHtlcVpkt()
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, packet.Serialize(&encoded))

	params.Amount++
	_, err = NewSwapKit(AssetHTLCV0, params)
	require.ErrorContains(t, err, "amount exceeds maximum")
}

// TestNewSwapKitRejectsSharedPartyKeys checks every sender/receiver role pair
// rejects the same x-only identity, including opposite-parity encodings.
func TestNewSwapKitRejectsSharedPartyKeys(t *testing.T) {
	for _, senderInternal := range []bool{false, true} {
		for _, receiverInternal := range []bool{false, true} {
			for _, oppositeParity := range []bool{false, true} {
				params := vectorParams(t)
				_, params.SenderInternalPubKey = scalarKey(t, 3)
				_, params.ReceiverInternalPubKey = scalarKey(t, 4)
				sharedKey := params.SenderScriptKey
				if senderInternal {
					sharedKey = params.SenderInternalPubKey
				}
				encoded := sharedKey.SerializeCompressed()
				if oppositeParity {
					encoded[0] ^= 1
				}
				receiverKey, err := btcec.ParsePubKey(encoded)
				require.NoError(t, err)
				if oppositeParity {
					require.False(t, sharedKey.IsEqual(receiverKey))
				}
				if receiverInternal {
					params.ReceiverInternalPubKey = receiverKey
				} else {
					params.ReceiverScriptKey = receiverKey
				}
				_, err = NewSwapKit(AssetHTLCV0, params)
				require.ErrorContains(t, err, "keys must differ")
			}
		}
	}

	// Each party may still reuse its own key to preserve legacy contracts.
	_, err := NewSwapKit(AssetHTLCV0, vectorParams(t))
	require.NoError(t, err)
}
