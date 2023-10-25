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
func (s *AddressServer) NewAddress(ctx context.Context,
	_ *looprpc.NewAddressRequest) (*looprpc.NewAddressResponse, error) {

	address, err := s.manager.NewAddress(ctx)
	if err != nil {
		return nil, err
	}

	log.Infof("New static loop-in address: %s\n", address.String())

	return &looprpc.NewAddressResponse{
		Address: address.String(),
	}, nil
}

// ListUnspent returns a list of utxos behind the static address.
func (s *AddressServer) ListUnspent(ctx context.Context,
	req *looprpc.ListUnspentRequest) (*looprpc.ListUnspentResponse, error) {

	// List all unspent utxos the wallet sees, regardless of the number of
	// confirmations.
	staticAddress, utxos, err := s.manager.ListUnspentRaw(
		ctx, req.MinConfs, req.MaxConfs,
	)
	if err != nil {
		return nil, err
	}

	// Prepare the list response.
	var respUtxos []*looprpc.Utxo
	for _, u := range utxos {
		utxo := &looprpc.Utxo{
			StaticAddress: staticAddress.String(),
			AmountSat:     int64(u.Value),
			Confirmations: u.Confirmations,
			Outpoint:      u.OutPoint.String(),
		}
		respUtxos = append(respUtxos, utxo)
	}

	return &looprpc.ListUnspentResponse{Utxos: respUtxos}, nil
}
