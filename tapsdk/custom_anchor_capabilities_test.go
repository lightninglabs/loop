package tapsdk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustomAnchorCapabilityStatusString(t *testing.T) {
	tests := []struct {
		name   string
		status CustomAnchorCapabilityStatus
		want   string
	}{
		{
			name:   "supported",
			status: CustomAnchorCapabilitySupported,
			want:   "supported",
		},
		{
			name:   "unsupported",
			status: CustomAnchorCapabilityUnsupported,
			want:   "unsupported",
		},
		{
			name:   unknownString,
			status: CustomAnchorCapabilityUnknown,
			want:   unknownString,
		},
		{
			name:   "unrecognized",
			status: CustomAnchorCapabilityStatus(99),
			want:   unknownString,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.status.String())
			require.Equal(
				t, tc.status == CustomAnchorCapabilitySupported,
				tc.status.Supported(),
			)
		})
	}
}

func TestCustomAnchorCapabilitiesSupport(t *testing.T) {
	caps := DefaultTapdCustomAnchorCapabilities()

	tests := []struct {
		name       string
		capability CustomAnchorCapability
		want       CustomAnchorCapabilityStatus
	}{
		{
			name:       "custom anchor commit",
			capability: CustomAnchorCapabilityCommit,
			want:       CustomAnchorCapabilitySupported,
		},
		{
			name:       "caller anchor psbt",
			capability: CustomAnchorCapabilityCallerAnchorPSBT,
			want:       CustomAnchorCapabilitySupported,
		},
		{
			name:       "skip funding",
			capability: CustomAnchorCapabilitySkipFunding,
			want:       CustomAnchorCapabilitySupported,
		},
		{
			name:       "no change output",
			capability: CustomAnchorCapabilityNoChangeOutput,
			want:       CustomAnchorCapabilityUnknown,
		},
		{
			name:       "local verification",
			capability: CustomAnchorCapabilityLocalVerification,
			want:       CustomAnchorCapabilityUnsupported,
		},
		{
			name:       "unknown capability",
			capability: CustomAnchorCapability("future"),
			want:       CustomAnchorCapabilityUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, caps.Support(tc.capability))
		})
	}
}

func TestCustomAnchorCapabilitiesRequire(t *testing.T) {
	caps := CustomAnchorCapabilities{
		BackendMode:              CustomAnchorBackendHosted,
		CustomAnchorCommit:       CustomAnchorCapabilitySupported,
		CallerSuppliedAnchorPSBT: CustomAnchorCapabilitySupported,
		SkipBroadcast:            CustomAnchorCapabilityUnsupported,
	}

	err := caps.Require(
		CustomAnchorCapabilityCommit,
		CustomAnchorCapabilityCallerAnchorPSBT,
	)
	require.NoError(t, err)

	err = caps.Require(CustomAnchorCapabilitySkipBroadcast)
	require.ErrorIs(t, err, ErrUnsupportedCustomAnchorCapability)

	var unsupported *UnsupportedCustomAnchorCapabilityError
	require.ErrorAs(t, err, &unsupported)
	require.Equal(t, CustomAnchorBackendHosted, unsupported.BackendMode)
	require.Equal(
		t, CustomAnchorCapabilitySkipBroadcast,
		unsupported.Capability,
	)
	require.Equal(
		t, CustomAnchorCapabilityUnsupported,
		unsupported.Status,
	)
	require.Contains(t, err.Error(), "skip_broadcast")
}

func TestCustomAnchorCapabilitiesRequireUnknown(t *testing.T) {
	caps := CustomAnchorCapabilities{
		BackendMode:        CustomAnchorBackendRemoteTapd,
		CustomAnchorCommit: CustomAnchorCapabilitySupported,
	}

	err := caps.Require(CustomAnchorCapabilityProofRegistration)
	require.ErrorIs(t, err, ErrUnsupportedCustomAnchorCapability)

	var unsupported *UnsupportedCustomAnchorCapabilityError
	require.ErrorAs(t, err, &unsupported)
	require.Equal(
		t, CustomAnchorCapabilityProofRegistration,
		unsupported.Capability,
	)
	require.Equal(t, CustomAnchorCapabilityUnknown, unsupported.Status)
}

func TestWalletCustomAnchorCapabilities(t *testing.T) {
	ctx := context.Background()
	want := DefaultTapdCustomAnchorCapabilities()
	mc := new(mockClient)
	mc.On("CustomAnchorCapabilities", ctx).Return(&want, nil)

	wallet := NewWallet(mc, NetworkRegtest)
	got, err := wallet.CustomAnchorCapabilities(ctx)
	require.NoError(t, err)
	require.Equal(t, &want, got)

	mc.AssertExpectations(t)
}

// clientWithoutCapabilityProvider intentionally hides optional methods on the
// wrapped Client so the Wallet's fail-closed fallback can be tested.
type clientWithoutCapabilityProvider struct {
	Client
}

func TestWalletCustomAnchorCapabilitiesUnknown(t *testing.T) {
	wallet := NewWallet(
		&clientWithoutCapabilityProvider{Client: new(mockClient)},
		NetworkRegtest,
	)

	got, err := wallet.CustomAnchorCapabilities(context.Background())
	require.NoError(t, err)
	require.Equal(t, &CustomAnchorCapabilities{}, got)

	err = got.Require(CustomAnchorCapabilityCommit)
	require.ErrorIs(t, err, ErrUnsupportedCustomAnchorCapability)
}
