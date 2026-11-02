package tapsdk

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestNewReceiveAddress_DefaultsToV2GroupKey(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)

	groupKey := testKey(t, 21)
	ref := AssetRefFromGroupKey(groupKey)

	expectedAddr := &Address{Encoded: testTap1example}
	mc.On("NewAddr", ctx, mock.MatchedBy(func(
		req *NewAddressRequest) bool {

		if req == nil || req.AddressVersion == nil {
			return false
		}

		return req.AssetRef == ref &&
			*req.AddressVersion ==
				AddressVersionV2 &&
			req.ProofCourierAddr == ""
	})).Return(expectedAddr, nil)

	wallet := NewWallet(mc, NetworkRegtest)

	addr, err := wallet.NewReceiveAddress(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, expectedAddr, addr)

	mc.AssertExpectations(t)
}

func TestNewReceiveAddress_CollectibleRetriesWithAmountOne(t *testing.T) {
	var assetID AssetID
	copy(assetID[:], []byte("collectible_asset_id_32_bytes_now"))

	groupKey := testKey(t, 22)

	tests := []struct {
		name string
		ref  AssetRef
	}{
		{
			name: "item asset ID ref",
			ref:  AssetRefFromAssetID(assetID),
		},
		{
			name: "collection group ref",
			ref:  AssetRefFromGroupKey(groupKey),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mc := new(mockClient)

			mc.On("NewAddr", ctx, mock.MatchedBy(func(
				req *NewAddressRequest) bool {

				if req == nil || req.AddressVersion == nil {
					return false
				}

				return req.AssetRef == tc.ref && req.Amount == 0 &&
					*req.AddressVersion ==
						AddressVersionV2
			})).Return((*Address)(nil), errors.New(
				"unable to make new addr: address: "+
					"collectible asset amount not one",
			)).Once()

			expectedAddr := &Address{
				Encoded: "tap1collectible",
				Amount:  1,
			}
			mc.On("NewAddr", ctx, mock.MatchedBy(func(
				req *NewAddressRequest) bool {

				if req == nil || req.AddressVersion == nil {
					return false
				}

				return req.AssetRef == tc.ref && req.Amount == 1 &&
					*req.AddressVersion ==
						AddressVersionV2
			})).Return(expectedAddr, nil).Once()

			wallet := NewWallet(mc, NetworkRegtest)

			addr, err := wallet.NewReceiveAddress(ctx, tc.ref)
			require.NoError(t, err)
			require.Equal(t, expectedAddr, addr)

			mc.AssertExpectations(t)
		})
	}
}

func TestNewReceiveAddress_UsesDefaultProofCourierAddr(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)

	groupKey := testKey(t, 23)
	ref := AssetRefFromGroupKey(groupKey)

	courierAddr := "authmailbox+universerpc://tapd.example:10029"
	expectedAddr := &Address{Encoded: testTap1example}
	mc.On("NewAddr", ctx, mock.MatchedBy(func(
		req *NewAddressRequest) bool {

		if req == nil || req.AddressVersion == nil {
			return false
		}

		return req.AssetRef == ref &&
			*req.AddressVersion ==
				AddressVersionV2 &&
			req.ProofCourierAddr == courierAddr
	})).Return(expectedAddr, nil)

	wallet := NewWallet(
		mc, NetworkRegtest,
		WithDefaultProofCourierAddr(courierAddr),
	)

	addr, err := wallet.NewReceiveAddress(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, expectedAddr, addr)

	mc.AssertExpectations(t)
}

func TestNewReceiveAddress_UsesAuthMailboxCourier(t *testing.T) {
	ctx := context.Background()
	mc := new(mockClient)

	groupKey := testKey(t, 24)
	ref := AssetRefFromGroupKey(groupKey)

	host := "tapd.example:10029"
	proofCourierAddr := "authmailbox+universerpc://" + host
	expectedAddr := &Address{Encoded: testTap1example}
	mc.On("NewAddr", ctx, mock.MatchedBy(func(
		req *NewAddressRequest) bool {

		if req == nil || req.AddressVersion == nil {
			return false
		}

		return req.AssetRef == ref &&
			*req.AddressVersion ==
				AddressVersionV2 &&
			req.ProofCourierAddr == proofCourierAddr
	})).Return(expectedAddr, nil)

	wallet := NewWallet(
		mc, NetworkRegtest,
		WithAuthMailboxCourier(host),
	)

	addr, err := wallet.NewReceiveAddress(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, expectedAddr, addr)

	mc.AssertExpectations(t)
}
