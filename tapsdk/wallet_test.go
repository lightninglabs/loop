package tapsdk

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestNewWalletRejectsUnknownNetwork(t *testing.T) {
	t.Parallel()

	require.PanicsWithError(t, "unsupported network: unknown", func() {
		_ = NewWallet(nil, Network(unknownString))
	})
}

func TestNewReceiveAddress_UsesGroupKeyAndV2(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	groupKey := PubKey{2, 1, 2, 3}
	ref := AssetRefFromGroupKey(groupKey)
	expectedAddr := &Address{
		Encoded:        "taprt1qqtestaddress",
		AssetRef:       ref,
		AddressVersion: AddressVersionV2,
	}

	mc.On("NewAddr", ctx, mock.MatchedBy(
		func(req *NewAddressRequest) bool {
			if req == nil ||
				req.AddressVersion == nil {

				return false
			}

			return req.AssetRef == ref &&
				req.Amount == 0 &&
				*req.AddressVersion ==
					AddressVersionV2
		},
	)).Return(expectedAddr, nil)

	addr, err := w.NewReceiveAddress(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, expectedAddr, addr)

	mc.AssertExpectations(t)
}

func TestNewReceiveAddress_RPCError(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	groupKey := PubKey{2, 9, 9, 9}
	ref := AssetRefFromGroupKey(groupKey)
	expectedErr := errors.New("new address failed")

	mc.On("NewAddr", ctx, mock.Anything).Return(
		nil, expectedErr,
	)

	_, err := w.NewReceiveAddress(ctx, ref)
	require.Error(t, err)
	require.ErrorIs(t, err, expectedErr)

	var sdkErr *Error
	require.ErrorAs(t, err, &sdkErr)
	require.Equal(t, "NewReceiveAddress", sdkErr.Op)

	mc.AssertExpectations(t)
}

func TestDeriveKeys(t *testing.T) {
	ctx := context.Background()

	scriptKey := &ScriptKey{
		PubKey: PubKey{2, 3, 4, 5},
		KeyDesc: KeyDescriptor{
			RawKeyBytes: PubKey{2, 6, 7, 8},
			KeyLocator: KeyLocator{
				Family: TaprootAssetsKeyFamily,
				Index:  7,
			},
		},
	}
	internalKey := &InternalKey{
		PubKey: PubKey{2, 10, 11, 12},
		KeyLocator: KeyLocator{
			Family: TaprootAssetsKeyFamily,
			Index:  8,
		},
	}

	tests := []struct {
		name        string
		scriptKey   *ScriptKey
		scriptErr   error
		internalKey *InternalKey
		internalErr error
	}{
		{
			name:        "success",
			scriptKey:   scriptKey,
			internalKey: internalKey,
		},
		{
			name:      "script key error",
			scriptErr: errors.New("derive script key failed"),
		},
		{
			name:        "internal key error",
			scriptKey:   scriptKey,
			internalErr: errors.New("derive internal key failed"),
		},
	}

	for _, testCase := range tests {
		tc := testCase
		t.Run(tc.name, func(t *testing.T) {
			mc := new(mockClient)
			w := NewWallet(mc, NetworkRegtest)

			mc.On("DeriveScriptKey", ctx).Return(
				tc.scriptKey, tc.scriptErr,
			)
			if tc.scriptErr == nil {
				mc.On(
					"DeriveInternalKey", ctx,
				).Return(
					tc.internalKey, tc.internalErr,
				)
			}

			derived, err := w.DeriveKeys(ctx)
			if tc.scriptErr != nil ||
				tc.internalErr != nil {

				require.Error(t, err)

				var sdkErr *Error
				require.ErrorAs(t, err, &sdkErr)
				require.Equal(
					t, "DeriveKeys", sdkErr.Op,
				)

				if tc.scriptErr != nil {
					require.ErrorIs(
						t, err, tc.scriptErr,
					)
				} else {
					require.ErrorIs(
						t, err, tc.internalErr,
					)
				}
			} else {
				require.NoError(t, err)
				require.Equal(
					t, &DerivedKeys{
						ScriptKey:   *scriptKey,
						InternalKey: *internalKey,
					}, derived,
				)
			}

			mc.AssertExpectations(t)
		})
	}
}

func TestWalletClose(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)

	expectedErr := errors.New("close failed")
	mc.On("Close").Return(expectedErr)

	err := w.Close()
	require.ErrorIs(t, err, expectedErr)

	mc.AssertExpectations(t)
}
