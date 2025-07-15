package deposit

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/taproot-assets/asset"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// ErrAssetDepositsUnavailable is returned when the asset deposit
	// service is not available.
	ErrAssetDepositsUnavailable = status.Error(codes.Unavailable,
		"asset deposits are unavailable")
)

// Server is the grpc server that serves the reservation service.
type Server struct {
	looprpc.UnimplementedAssetDepositClientServer

	manager *Manager
}

func NewServer(manager *Manager) *Server {
	return &Server{
		manager: manager,
	}
}

// NewAssetDeposit is the rpc endpoint for loop clients to request a new asset
// deposit.
func (s *Server) NewAssetDeposit(ctx context.Context,
	in *looprpc.NewAssetDepositRequest) (*looprpc.NewAssetDepositResponse,
	error) {

	if s.manager == nil {
		return nil, ErrAssetDepositsUnavailable
	}

	assetIDBytes, err := hex.DecodeString(in.AssetId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument,
			fmt.Sprintf("invalid asset ID encoding: %v", err))
	}

	var assetID asset.ID
	if len(assetIDBytes) != len(assetID) {
		return nil, fmt.Errorf("invalid asset ID lenght: expected "+
			"%v bytes, got %d", len(assetID), len(assetIDBytes))
	}

	copy(assetID[:], assetIDBytes)

	if in.Amount == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"amount must be greater than zero")
	}

	if in.CsvExpiry <= 0 {
		return nil, status.Error(codes.InvalidArgument,
			"CSV expiry must be greater than zero")
	}

	depositInfo, err := s.manager.NewDeposit(
		ctx, assetID, in.Amount, uint32(in.CsvExpiry),
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &looprpc.NewAssetDepositResponse{
		DepositId: depositInfo.ID,
	}, nil
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

// TestCoSignAssetDepositHTLC is the rpc endpoint for loop clients to test
// co-signing an asset deposit HTLC.
func (s *Server) TestCoSignAssetDepositHTLC(ctx context.Context,
	in *looprpc.TestCoSignAssetDepositHTLCRequest) (
	*looprpc.TestCoSignAssetDepositHTLCResponse, error) {

	return nil, status.Error(codes.Unimplemented, "unimplemented")
}
