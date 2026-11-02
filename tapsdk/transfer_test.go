package tapsdk

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewTransferAssetRefsUseAssetType(t *testing.T) {
	t.Parallel()

	groupKey := testGroupKey(t)
	assetID := testAssetID()

	tests := []struct {
		name      string
		assetType AssetType
		groupKey  *PubKey
		wantRef   AssetRef
	}{
		{
			name:      "grouped fungible",
			assetType: AssetTypeNormal,
			groupKey:  &groupKey,
			wantRef:   AssetRefFromGroupKey(groupKey),
		},
		{
			name:      "ungrouped fungible",
			assetType: AssetTypeNormal,
			wantRef:   AssetRefFromAssetID(assetID),
		},
		{
			name:      "grouped collectible",
			assetType: AssetTypeCollectible,
			groupKey:  &groupKey,
			wantRef:   AssetRefFromAssetID(assetID),
		},
		{
			name:      "standalone collectible",
			assetType: AssetTypeCollectible,
			wantRef:   AssetRefFromAssetID(assetID),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := &AssetTransfer{
				Inputs: []TransferInput{{
					IssuanceID: assetID,
					AssetType:  tc.assetType,
					GroupKey:   tc.groupKey,
				}},
				Outputs: []TransferOutput{{
					IssuanceID: assetID,
					AssetType:  tc.assetType,
					GroupKey:   tc.groupKey,
				}},
			}

			transfer := NewTransfer(raw)
			require.NotNil(t, transfer)
			require.Len(t, transfer.Inputs, 1)
			require.Len(t, transfer.Outputs, 1)

			require.True(t,
				transfer.Inputs[0].AssetRef.Equivalent(tc.wantRef),
			)
			require.Equal(t, tc.assetType, transfer.Inputs[0].Type)

			require.True(t,
				transfer.Outputs[0].AssetRef.Equivalent(tc.wantRef),
			)
			require.Equal(t, tc.assetType, transfer.Outputs[0].Type)
		})
	}
}

func TestRecipientAmount(t *testing.T) {
	explicit := RecipientWithAmount("tap1explicit", 42)
	amount, ok := explicit.Amount()
	require.True(t, ok)
	require.Equal(t, uint64(42), amount)
	require.Equal(t, "tap1explicit", explicit.Address)

	embedded := RecipientWithEmbeddedAmount("tap1embedded")
	amount, ok = embedded.Amount()
	require.False(t, ok)
	require.Zero(t, amount)
	require.Equal(t, "tap1embedded", embedded.Address)
}
