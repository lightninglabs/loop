package loop

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type proofSizeServer struct {
	swapserverrpc.UnimplementedAssetLoopInServerServer
	swapserverrpc.UnimplementedStaticAddressServerServer

	calls atomic.Int32
}

func (s *proofSizeServer) ServerStaticAddressLoopIn(_ context.Context,
	r *swapserverrpc.ServerStaticAddressLoopInRequest) (
	*swapserverrpc.ServerStaticAddressLoopInResponse, error) {

	s.calls.Add(1)
	return &swapserverrpc.ServerStaticAddressLoopInResponse{
		AssetStatus: &swapserverrpc.AssetLoopInStatus{
			ResolutionProof: r.AssetParams.Contract,
		},
	}, nil
}

func (s *proofSizeServer) GetAssetLoopIn(context.Context,
	*swapserverrpc.AssetLoopInReference) (
	*swapserverrpc.AssetLoopInStatus, error) {

	return &swapserverrpc.AssetLoopInStatus{
		ResolutionProof: make([]byte, bridge.PublicRPCMaxBytes),
	}, nil
}

// TestSwapServerProofMessageLimits exercises the normal authenticated client
// connection with proofs above gRPC's default limit and both rejection bounds.
func TestSwapServerProofMessageLimits(t *testing.T) {
	// Keep the socket below the platform's Unix socket path length limit.
	dir, err := os.MkdirTemp("", "loop-rpc-") //nolint:usetesting
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	socket := filepath.Join(dir, "rpc.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(2*bridge.PublicRPCMaxBytes),
		grpc.MaxSendMsgSize(2*bridge.PublicRPCMaxBytes),
	)
	service := &proofSizeServer{}
	swapserverrpc.RegisterAssetLoopInServerServer(server, service)
	swapserverrpc.RegisterStaticAddressServerServer(server, service)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() { server.Stop(); <-done })
	tokens, err := l402.NewFileStore(t.TempDir())
	require.NoError(t, err)
	auth := newL402ClientInterceptor(nil, tokens, 5*time.Second, 0, 0, true)
	conn, err := getSwapServerConn("unix://"+socket, "", true, "", auth)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	client := swapserverrpc.NewStaticAddressServerClient(conn)
	assetClient := swapserverrpc.NewAssetLoopInServerClient(conn)
	proof := make([]byte, 8<<20)
	response, err := client.ServerStaticAddressLoopIn(t.Context(),
		&swapserverrpc.ServerStaticAddressLoopInRequest{
			AssetParams: &swapserverrpc.CreateAssetLoopInRequest{
				Contract: proof,
			},
		})
	require.NoError(t, err)
	require.Equal(t, proof, response.AssetStatus.ResolutionProof)
	_, err = client.ServerStaticAddressLoopIn(t.Context(),
		&swapserverrpc.ServerStaticAddressLoopInRequest{
			AssetParams: &swapserverrpc.CreateAssetLoopInRequest{
				Contract: make([]byte, bridge.PublicRPCMaxBytes),
			},
		})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.EqualValues(t, 1, service.calls.Load())
	_, err = assetClient.GetAssetLoopIn(t.Context(),
		&swapserverrpc.AssetLoopInReference{})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
}
