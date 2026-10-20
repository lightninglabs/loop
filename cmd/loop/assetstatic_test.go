package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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

type assetPermissionClient struct {
	looprpc.SwapClientClient
	request  *looprpc.StaticAddressLoopInRequest
	response *looprpc.StaticAddressLoopInResponse
	err      error
}

// GetInfo deliberately denies the unrelated permission to guard against
// reintroducing a capability preflight on the asset execution path.
func (*assetPermissionClient) GetInfo(context.Context,
	*looprpc.GetInfoRequest, ...grpc.CallOption) (
	*looprpc.GetInfoResponse, error) {

	return nil, status.Error(codes.PermissionDenied, "suggestions denied")
}

func (c *assetPermissionClient) StaticAddressLoopIn(_ context.Context,
	request *looprpc.StaticAddressLoopInRequest, _ ...grpc.CallOption) (
	*looprpc.StaticAddressLoopInResponse, error) {

	c.request = request
	return c.response, c.err
}

// TestAssetExecutionNeedsOnlyItsOwnPermission verifies that creation needs no
// GetInfo permission and that an older daemon sees no Bitcoin selection.
func TestAssetExecutionNeedsOnlyItsOwnPermission(t *testing.T) {
	id := [32]byte{1}
	params := &looprpc.AssetLoopInParams{
		RequestId: id[:], Outpoint: strings.Repeat("ab", 32) + ":0",
		Amount: 1000, MaxAssetDebit: 1000,
	}
	client := &assetPermissionClient{
		response: &looprpc.StaticAddressLoopInResponse{
			AssetRequest: &looprpc.AssetRequestStatus{
				RequestId: id[:], State: "queued",
			},
		},
	}
	_, err := client.GetInfo(t.Context(), &looprpc.GetInfoRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	response, err := assetLoopInRPC(t.Context(), client, params)
	require.NoError(t, err)
	require.Equal(t, id[:], response.AssetRequest.RequestId)
	require.True(t, proto.Equal(client.request,
		&looprpc.StaticAddressLoopInRequest{AssetParams: params}))
	require.Empty(t, client.request.Outpoints)
	require.Zero(t, client.request.Amount)

	for _, legacy := range []*looprpc.StaticAddressLoopInResponse{
		nil, {}, {SwapHash: id[:]},
	} {
		client.response = legacy
		_, err := assetLoopInRPC(t.Context(), client, params)
		require.ErrorContains(t, err, "did not accept the asset protocol")
	}
	client.err = errors.New("daemon unavailable")
	_, err = assetLoopInRPC(t.Context(), client, params)
	require.ErrorIs(t, err, client.err)
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

type assetListConnection struct {
	daemonConn
	t      *testing.T
	method string
	legacy bool
}

func (c *assetListConnection) Invoke(_ context.Context, method string,
	request, response any, _ ...grpc.CallOption) error {

	// A narrowly scoped macaroon authorizes only this listing. In
	// particular, GetInfo's suggestions permission is unavailable.
	if method != c.method {
		return status.Error(codes.PermissionDenied, "method denied")
	}
	switch req := request.(type) {
	case *looprpc.ListStaticAddressDepositsRequest:
		require.True(c.t, req.AssetsOnly)
		response.(*looprpc.ListStaticAddressDepositsResponse).
			AssetsOnly = !c.legacy
	case *looprpc.ListStaticAddressSwapsRequest:
		require.True(c.t, req.AssetsOnly)
		response.(*looprpc.ListStaticAddressSwapsResponse).
			AssetsOnly = !c.legacy
	default:
		c.t.Fatalf("unexpected listing request %T", request)
	}
	return nil
}

type assetListTransport struct {
	grpcTransport
	conn daemonConn
}

func (t assetListTransport) Dial(*cli.Command) (daemonConn, func(), error) {
	return t.conn, func() {}, nil
}

// TestAssetListingsNeedOnlyTheirOwnPermission exercises the actual CLI
// actions with an empty asset listing and no unrelated RPC permission.
func TestAssetListingsNeedOnlyTheirOwnPermission(t *testing.T) {
	for _, test := range []struct {
		method string
		action func(context.Context, *cli.Command) error
	}{
		{"ListStaticAddressDeposits", listAssetDeposits},
		{"ListStaticAddressSwaps", listAssetSwaps},
	} {
		t.Run(test.method, func(t *testing.T) {
			conn := &assetListConnection{
				t: t, method: "/looprpc.SwapClient/" + test.method,
			}
			t.Cleanup(hookGrpc(assetListTransport{conn: conn}))
			require.NoError(t, test.action(t.Context(), &cli.Command{}))
			conn.legacy = true
			err := test.action(t.Context(), &cli.Command{})
			require.ErrorContains(t, err,
				"did not accept the asset protocol")
		})
	}
}
