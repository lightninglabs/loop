package deposit

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server is the grpc server that serves the reservation service.
type Server struct {
	looprpc.UnimplementedAssetDepositClientServer
}

func NewServer() *Server {
	return &Server{}
}

// NewAssetDeposit is the rpc endpoint for loop clients to request a new asset
// deposit.
func (s *Server) NewAssetDeposit(ctx context.Context,
	in *looprpc.NewAssetDepositRequest) (*looprpc.NewAssetDepositResponse,
	error) {

	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

// ListAssetDeposits is the rpc endpoint for loop clients to list their asset
// deposits.
func (s *Server) ListAssetDeposits(ctx context.Context,
	in *looprpc.ListAssetDepositsRequest) (
	*looprpc.ListAssetDepositsResponse, error) {

	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

// RevealAssetDepositKey is the rpc endpoint for loop clients to reveal the
// asset deposit key for a specific asset deposit.
func (s *Server) RevealAssetDepositKey(ctx context.Context,
	in *looprpc.RevealAssetDepositKeyRequest) (
	*looprpc.RevealAssetDepositKeyResponse, error) {

	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

// WithdrawAssetDeposits is the rpc endpoint for loop clients to withdraw their
// asset deposits.
func (s *Server) WithdrawAssetDeposits(ctx context.Context,
	in *looprpc.WithdrawAssetDepositsRequest) (
	*looprpc.WithdrawAssetDepositsResponse, error) {

	return nil, status.Error(codes.Unimplemented, "unimplemented")
}
