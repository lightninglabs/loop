package assetclient

import (
	"context"

	"github.com/lightninglabs/loop/swapserverrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server exposes the asset protocol operations to the durable controller.
// Creation and commitment use the existing static-address RPCs on the wire.
type Server interface {
	swapserverrpc.AssetLoopInServerClient

	CreateAssetLoopIn(context.Context,
		*swapserverrpc.CreateAssetLoopInRequest, ...grpc.CallOption) (
		*swapserverrpc.AssetLoopInStatus, error)
	CommitAssetLoopIn(context.Context,
		*swapserverrpc.CommitAssetLoopInRequest, ...grpc.CallOption) (
		*swapserverrpc.AssetLoopInStatus, error)
}

type serverClient struct {
	swapserverrpc.AssetLoopInServerClient

	static swapserverrpc.StaticAddressServerClient
}

// NewServerClient binds the asset variant of the existing static RPCs and the
// additional payout/recovery phases to the same authenticated connection.
func NewServerClient(conn grpc.ClientConnInterface) Server {
	return &serverClient{
		AssetLoopInServerClient: swapserverrpc.NewAssetLoopInServerClient(
			conn,
		),
		static: swapserverrpc.NewStaticAddressServerClient(conn),
	}
}

// CreateAssetLoopIn preserves the exact inner request used for the durable
// digest. Bitcoin fields remain empty so an old server cannot execute a swap.
func (s *serverClient) CreateAssetLoopIn(ctx context.Context,
	request *swapserverrpc.CreateAssetLoopInRequest,
	opts ...grpc.CallOption) (*swapserverrpc.AssetLoopInStatus, error) {

	response, err := s.static.ServerStaticAddressLoopIn(ctx,
		&swapserverrpc.ServerStaticAddressLoopInRequest{
			ProtocolVersion: swapserverrpc.StaticAddressProtocolVersion_ASSET_V1,
			AssetParams:     request,
		}, opts...)
	if err != nil {
		return nil, err
	}
	if response.GetAssetStatus() == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"server did not accept the asset protocol")
	}
	return response.AssetStatus, nil
}

// CommitAssetLoopIn shares conditional signatures without granting the client
// control over the server's execution or preimage disclosure.
func (s *serverClient) CommitAssetLoopIn(ctx context.Context,
	request *swapserverrpc.CommitAssetLoopInRequest,
	opts ...grpc.CallOption) (*swapserverrpc.AssetLoopInStatus, error) {

	response, err := s.static.PushStaticAddressHtlcSigs(ctx,
		&swapserverrpc.PushStaticAddressHtlcSigsRequest{
			ProtocolVersion: swapserverrpc.StaticAddressProtocolVersion_ASSET_V1,
			AssetParams:     request,
		}, opts...)
	if err != nil {
		return nil, err
	}
	if response.GetAssetStatus() == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"server did not accept the asset protocol")
	}
	return response.AssetStatus, nil
}
