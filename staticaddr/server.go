package staticaddr

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
)

// AddressServer holds all fields for the address rpc server.
type AddressServer struct {
	addressClient staticaddressrpc.StaticAddressServerClient
	manager       *Manager
	looprpc.UnimplementedStaticAddressClientServer
}

// NewAddressServer creates a new static address server.
func NewAddressServer(addressClient staticaddressrpc.StaticAddressServerClient,
	manager *Manager) *AddressServer {

	return &AddressServer{
		addressClient: addressClient,
		manager:       manager,
	}
}

// NewAddress is the rpc endpoint for loop clients to request a new static
// address.
func (r *AddressServer) NewAddress(ctx context.Context,
	_ *looprpc.NewAddressRequest) (*looprpc.NewAddressResponse, error) {

	address, err := r.manager.NewAddress(ctx)
	if err != nil {
		return nil, err
	}

	log.Infof("New static loop-in address: %s\n", address.String())

	return &looprpc.NewAddressResponse{
		Address: address.String(),
	}, nil
}
