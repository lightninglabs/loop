package server

import (
	"context"

	"github.com/lightninglabs/loop/swapserverrpc"
)

// FetchL402 intentionally has no application-level payload. When this server
// is placed behind Aperture, the proxy turns this call into the L402 challenge
// that binds a static address to the regtest client.
func (s *Server) FetchL402(context.Context,
	*swapserverrpc.FetchL402Request) (*swapserverrpc.FetchL402Response, error) {

	return &swapserverrpc.FetchL402Response{}, nil
}

// RecommendRoutingPlugin keeps the demo independent from optional routing
// plugins. lnd's normal payment router is sufficient for the two-node regtest
// topology.
func (s *Server) RecommendRoutingPlugin(context.Context,
	*swapserverrpc.RecommendRoutingPluginReq) (
	*swapserverrpc.RecommendRoutingPluginRes, error) {

	return &swapserverrpc.RecommendRoutingPluginRes{
		Plugin: swapserverrpc.RoutingPlugin_NONE,
	}, nil
}

func (s *Server) ReportRoutingResult(context.Context,
	*swapserverrpc.ReportRoutingResultReq) (
	*swapserverrpc.ReportRoutingResultRes, error) {

	return &swapserverrpc.ReportRoutingResultRes{}, nil
}

// SubscribeNotifications is the long-lived control stream used by the static
// address managers. Aperture authenticates the stream before it reaches us.
func (s *Server) SubscribeNotifications(
	_ *swapserverrpc.SubscribeNotificationsRequest,
	stream swapserverrpc.SwapServer_SubscribeNotificationsServer) error {

	updates := s.notifications.subscribe(stream.Context())
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				return stream.Context().Err()
			}
			if err := stream.Send(update); err != nil {
				return err
			}

		case <-s.ctx.Done():
			return s.ctx.Err()

		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}
