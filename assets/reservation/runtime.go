package reservation

import (
	"context"
	"errors"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"google.golang.org/grpc"
)

// NewRuntime connects the existing client database and authenticated node
// services. The daemon owns Run and waits for recovery before serving RPCs.
func NewRuntime(db *loopdb.BaseDB, lnd *lndclient.LndServices,
	tap ProofClient, server swapserverrpc.AssetReservationServiceClient,
	notify func(ID, error)) (*Manager, error) {

	if db == nil || lnd == nil || lnd.ClientConn == nil || tap == nil {
		return nil, errors.New("missing reservation runtime services")
	}
	params := address.ParamsForChain(lnd.ChainParams.Name)
	wallet := &WalletVerifier{
		Keys: lnd.WalletKit,
		Tap: tapProofVerifier{
			tap: tap,
		},
		Params:    &params,
		KeyFamily: swap.KeyFamily,
		Chain: &ChainReader{
			Node:          lnd.Client,
			Chain:         lnd.ChainKit,
			ChainNotifier: lnd.ChainNotifier,
		},
	}
	if err := wallet.Validate(); err != nil {
		return nil, err
	}
	payments, err := NewLndPayments(PaymentConfig{
		Node: lnrpc.NewLightningClient(&reservationConnection{
			lnd:     lnd,
			service: lndclient.ReadOnlyServiceMac,
		}),
		Router: routerrpc.NewRouterClient(&reservationConnection{
			lnd:     lnd,
			service: lndclient.RouterServiceMac,
		}),
		Params:         lnd.ChainParams,
		Now:            time.Now,
		PaymentTimeout: 30 * time.Second,
		ProbeTimeout:   10 * time.Second,
		CLTVLimit:      htlcswitch.DefaultMaxOutgoingCltvExpiry,
		MinFinalCLTV:   40,
	})
	if err != nil {
		return nil, err
	}
	return NewManager(ManagerConfig{
		FSM: &Config{
			Store:       NewSqlStore(db),
			Server:      server,
			Payments:    payments,
			Wallet:      wallet,
			Clock:       clock.NewDefaultClock(),
			NotifyAdmin: notify,
		},
		PollInterval:          time.Second,
		CallTimeout:           time.Minute,
		MaxActiveReservations: 100,
	})
}

// reservationConnection adds the existing service macaroon to native RPCs
// while preserving the manager's cancellation and deadline on every call.
type reservationConnection struct {
	lnd     *lndclient.LndServices
	service lndclient.LnrpcServiceMac
}

func (c *reservationConnection) Invoke(ctx context.Context, method string,
	args, reply any, options ...grpc.CallOption) error {

	ctx, err := c.lnd.WithMacaroonAuthForService(ctx, c.service)
	if err != nil {
		return err
	}
	return c.lnd.ClientConn.Invoke(ctx, method, args, reply, options...)
}

func (c *reservationConnection) NewStream(ctx context.Context,
	desc *grpc.StreamDesc, method string,
	options ...grpc.CallOption) (grpc.ClientStream, error) {

	ctx, err := c.lnd.WithMacaroonAuthForService(ctx, c.service)
	if err != nil {
		return nil, err
	}
	return c.lnd.ClientConn.NewStream(ctx, desc, method, options...)
}
