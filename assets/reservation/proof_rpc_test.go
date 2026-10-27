package reservation

import (
	"context"
	"net"
	"testing"

	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type reservationProofRPCServer struct {
	swapserverrpc.UnimplementedAssetReservationServiceServer

	response *swapserverrpc.AssetReservationProof
}

func (s *reservationProofRPCServer) GetAssetReservationProof(context.Context,
	*swapserverrpc.AssetReservationSelector) (
	*swapserverrpc.AssetReservationProof, error) {

	return s.response, nil
}

// TestReservationProofReceiveLimit exercises the FSM through real gRPC. The
// wallet double isolates transport and persistence from proof validation.
func TestReservationProofReceiveLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      int
		oversized bool
	}{
		{name: "above grpc default", size: 5 << 20},
		{name: "maximum proof plus metadata", size: maxReservationProofSize},
		{
			name: "oversized response", size: maxReservationProofMessageSize,
			oversized: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newClientHarness(t)
			h.event(OnRecover, nil)
			h.approve()
			h.settle()
			h.deliver = true
			h.event(OnRecover, nil)
			h.state(VerifyReservation)

			listener := bufconn.Listen(1 << 20)
			server := grpc.NewServer()
			response := &swapserverrpc.AssetReservationProof{
				ReservationId: h.id[:], Outpoint: h.outpoint.String(),
				Proof: make([]byte, tc.size),
			}
			swapserverrpc.RegisterAssetReservationServiceServer(server,
				&reservationProofRPCServer{response: response})
			serveErr := make(chan error, 1)
			go func() { serveErr <- server.Serve(listener) }()
			t.Cleanup(func() {
				server.Stop()
				require.NoError(t, <-serveErr)
			})
			conn, err := grpc.NewClient("passthrough:///reservation-proof",
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(ctx context.Context,
					_ string) (net.Conn, error) {

					return listener.DialContext(ctx)
				}),
			)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, conn.Close()) })
			h.cfg.Server = swapserverrpc.NewAssetReservationServiceClient(conn)
			h.status.Height = 102
			h.event(OnRecover, nil)
			if tc.oversized {
				require.Equal(t, codes.ResourceExhausted,
					status.Code(h.machine.LastActionError))
				h.state(VerifyReservation)
				require.Empty(t, h.record().ReservationProof)
				return
			}
			require.NoError(t, h.machine.LastActionError)
			h.state(Ready)
			require.Equal(t, response.Proof, h.record().ReservationProof)
		})
	}
}
