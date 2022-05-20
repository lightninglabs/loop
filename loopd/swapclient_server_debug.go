//go:build dev
// +build dev

package loopd

import (
	"context"
	"fmt"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/looprpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

var (
	debugRequiredPermissions = map[string][]bakery.Op{
		"/looprpc.Debug/ForceAutoLoop": {{
			Entity: "debug",
			Action: "write",
		}},
	}
)

// registerDebugServer registers the debug server.
func (d *Daemon) registerDebugServer() {
	looprpc.RegisterDebugServer(d.grpcServer, d)
}

// ForceAutoLoop triggers our liquidity manager to dispatch an automated swap,
// if one is suggested. This endpoint is only for testing purposes and cannot be
// used on mainnet.
func (s *swapClientServer) ForceAutoLoop(ctx context.Context,
	_ *looprpc.ForceAutoLoopRequest) (*looprpc.ForceAutoLoopResponse, error) {

	if s.network == lndclient.NetworkMainnet {
		return nil, fmt.Errorf("force autoloop not allowed on mainnet")
	}

	err := s.liquidityMgr.ForceAutoLoop(ctx)
	return &looprpc.ForceAutoLoopResponse{}, err
}
