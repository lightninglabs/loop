package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// TestConfirmationHeightForUtxo verifies confirmation height derivation from
// wallet UTXO confirmation counts.
func TestConfirmationHeightForUtxo(t *testing.T) {
	manager := NewManager(&ManagerConfig{})
	manager.currentHeight.Store(100)

	tests := []struct {
		name          string
		confirmations int64
		expected      int64
	}{
		{
			name:          "unconfirmed",
			confirmations: 0,
			expected:      0,
		},
		{
			name:          "single confirmation",
			confirmations: 1,
			expected:      100,
		},
		{
			name:          "three confirmations",
			confirmations: 3,
			expected:      98,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			height, err := manager.confirmationHeightForUtxo(
				&lnwallet.Utxo{
					Confirmations: test.confirmations,
					OutPoint: wire.OutPoint{
						Index: 1,
					},
				},
			)
			require.NoError(t, err)
			require.Equal(t, test.expected, height)
		})
	}
}
