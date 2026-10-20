package assetclient

import (
	"context"
	"testing"

	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type staticRPCConnection struct {
	grpc.ClientConnInterface

	invoke func(string, any, any) error
}

func (c staticRPCConnection) Invoke(_ context.Context, method string,
	request, response any, _ ...grpc.CallOption) error {

	return c.invoke(method, request, response)
}

// TestStaticAssetRPCTransport preserves the immutable inner request while
// selecting the asset protocol on the existing authenticated static service.
func TestStaticAssetRPCTransport(t *testing.T) {
	request, offer, _ := offerFixture(t)
	marshal := proto.MarshalOptions{Deterministic: true}
	original, err := marshal.Marshal(request)
	require.NoError(t, err)
	commit := &swapserverrpc.CommitAssetLoopInRequest{
		PaymentHash:      offer.Offer.PaymentHash,
		ClientSignatures: []byte("persisted conditional claim"),
	}
	calls := 0
	client := NewServerClient(staticRPCConnection{
		invoke: func(method string, req, resp any) error {
			calls++
			switch method {
			case "/looprpc.StaticAddressServer/ServerStaticAddressLoopIn":
				r := req.(*swapserverrpc.ServerStaticAddressLoopInRequest)
				require.True(t, proto.Equal(r,
					&swapserverrpc.ServerStaticAddressLoopInRequest{
						ProtocolVersion: swapserverrpc.StaticAddressProtocolVersion_ASSET_V1,
						AssetParams:     request,
					}))
				raw, err := marshal.Marshal(r.AssetParams)
				require.NoError(t, err)
				require.Equal(t, original, raw)
				resp.(*swapserverrpc.ServerStaticAddressLoopInResponse).
					AssetStatus = offer

			case "/looprpc.StaticAddressServer/PushStaticAddressHtlcSigs":
				r := req.(*swapserverrpc.PushStaticAddressHtlcSigsRequest)
				require.True(t, proto.Equal(r,
					&swapserverrpc.PushStaticAddressHtlcSigsRequest{
						ProtocolVersion: swapserverrpc.StaticAddressProtocolVersion_ASSET_V1,
						AssetParams:     commit,
					}))
				resp.(*swapserverrpc.PushStaticAddressHtlcSigsResponse).
					AssetStatus = offer
			default:
				t.Fatalf("unexpected asset RPC: %s", method)
			}
			return nil
		},
	})
	created, err := client.CreateAssetLoopIn(t.Context(), request)
	require.NoError(t, err)
	require.True(t, proto.Equal(offer, created))
	committed, err := client.CommitAssetLoopIn(t.Context(), commit)
	require.NoError(t, err)
	require.True(t, proto.Equal(offer, committed))
	require.Equal(t, 2, calls)
}

// TestStaticAssetRPCRejectsLegacyResponse does not accept a Bitcoin response
// from a peer that ignored the asset fields.
func TestStaticAssetRPCRejectsLegacyResponse(t *testing.T) {
	client := NewServerClient(staticRPCConnection{
		invoke: func(string, any, any) error { return nil },
	})
	_, err := client.CreateAssetLoopIn(t.Context(),
		&swapserverrpc.CreateAssetLoopInRequest{})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = client.CommitAssetLoopIn(t.Context(),
		&swapserverrpc.CommitAssetLoopInRequest{})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}
