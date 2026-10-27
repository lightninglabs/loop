package assets

import (
	"context"
	"testing"

	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestCheckOnchainSwapTapdVersion checks the on-chain swap floor against the
// version strings that tapd reports in GetInfo.
func TestCheckOnchainSwapTapdVersion(t *testing.T) {
	tests := []struct {
		name      string
		version   string
		supported bool
	}{
		{
			name:      "minimum release",
			version:   "0.8.6-alpha commit=v0.8.6",
			supported: true,
		},
		{
			name:      "prefixed minimum release",
			version:   "v0.8.6",
			supported: true,
		},
		{
			name:      "minimum release candidate",
			version:   "0.8.6-alpha.rc1 commit=v0.8.6-rc1",
			supported: true,
		},
		{
			name:      "newer patch",
			version:   "0.8.7-alpha",
			supported: true,
		},
		{
			name:      "newer minor",
			version:   "0.9.0-alpha",
			supported: true,
		},
		{
			name:      "newer major",
			version:   "1.0.0",
			supported: true,
		},
		{
			name:    "older patch",
			version: "0.8.5-alpha commit=v0.8.5",
		},
		{
			name:    "older minor",
			version: "0.7.9",
		},
		{
			name:    "empty",
			version: "",
		},
		{
			name:    "missing patch",
			version: "0.8",
		},
		{
			name:    "extra component",
			version: "0.8.6.1",
		},
		{
			name:    "negative component",
			version: "0.-8.6",
		},
		{
			name:    "not a version",
			version: "garbage",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := CheckOnchainSwapTapdVersion(test.version)
			if test.supported {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrTapdVersionUnsupported)
		})
	}
}

// TestMinOnchainSwapTapdVersion checks that the floor itself is a parseable
// release, as an invalid floor would reject every tapd version.
func TestMinOnchainSwapTapdVersion(t *testing.T) {
	require.Equal(t, "0.8.6", MinOnchainSwapTapdVersion)
	require.NoError(t, CheckOnchainSwapTapdVersion(
		MinOnchainSwapTapdVersion,
	))
}

type tapdInfoClient struct {
	version string
}

func (c tapdInfoClient) GetInfo(context.Context, *taprpc.GetInfoRequest,
	...grpc.CallOption) (*taprpc.GetInfoResponse, error) {

	return &taprpc.GetInfoResponse{Version: c.version}, nil
}

// TestRequireOnchainSwapTapd exercises the public runtime gate used by other
// applications before offering or funding a new swap through their own APIs.
func TestRequireOnchainSwapTapd(t *testing.T) {
	for _, version := range []string{"0.8.5-alpha", "0.8.6-alpha"} {
		t.Run(version, func(t *testing.T) {
			err := RequireOnchainSwapTapd(t.Context(), tapdInfoClient{
				version: version,
			})
			if version == "0.8.6-alpha" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrTapdVersionUnsupported)
		})
	}
	require.ErrorIs(t, RequireOnchainSwapTapd(t.Context(), nil),
		ErrTapdVersionUnsupported)
}
