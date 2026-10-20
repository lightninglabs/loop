package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestCanonicalAssetIdentifier(t *testing.T) {
	canonical := strings.Repeat("ab", 32)
	raw, err := canonicalAssetHex(canonical, 32)
	require.NoError(t, err)
	require.Len(t, raw, 32)
	for _, input := range []string{
		strings.ToUpper(canonical), "0x" + canonical, canonical[:62],
	} {
		_, err := canonicalAssetHex(input, 32)
		require.Error(t, err)
	}
}

type assetCapabilityClient struct {
	looprpc.SwapClientClient
	info *looprpc.GetInfoResponse
	err  error
}

func (c assetCapabilityClient) GetInfo(context.Context,
	*looprpc.GetInfoRequest, ...grpc.CallOption) (
	*looprpc.GetInfoResponse, error) {

	return c.info, c.err
}

// TestAssetCapabilityGate rejects old and disabled daemons before a reused
// static-address RPC could interpret an asset request as Bitcoin.
func TestAssetCapabilityGate(t *testing.T) {
	for _, info := range []*looprpc.GetInfoResponse{
		nil, {}, {AssetStaticLoopIn: false},
	} {
		err := requireAssetStaticLoopIn(t.Context(),
			assetCapabilityClient{info: info})
		require.ErrorContains(t, err, "does not support or has disabled")
	}
	require.NoError(t, requireAssetStaticLoopIn(t.Context(),
		assetCapabilityClient{info: &looprpc.GetInfoResponse{
			AssetStaticLoopIn: true,
		}}))
	unavailable := errors.New("daemon unavailable")
	require.ErrorIs(t, requireAssetStaticLoopIn(t.Context(),
		assetCapabilityClient{err: unavailable}), unavailable)
}

// TestRecordedAssetDefaults permits additive protobuf defaults without hiding
// an explicit asset request or an unknown recorded field.
func TestRecordedAssetDefaults(t *testing.T) {
	compare := func(request *looprpc.ListStaticAddressSwapsRequest,
		raw string) error {

		return compareMessageWithContext("ListStaticAddressSwaps",
			"request", 0, request, []byte(raw))
	}
	require.NoError(t, compare(&looprpc.ListStaticAddressSwapsRequest{}, "{}"))
	require.Error(t, compare(&looprpc.ListStaticAddressSwapsRequest{
		AssetsOnly: true,
	}, "{}"))
	require.Error(t, compare(&looprpc.ListStaticAddressSwapsRequest{},
		`{"unexpected": true}`))
}
