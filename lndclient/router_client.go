package lndclient

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing/route"

	"github.com/lightningnetwork/lnd/channeldb"
	"google.golang.org/grpc/codes"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
)

// RouterClient exposes payment functionality.
type RouterClient interface {
	// SendPayment attempts to route a payment to the final destination. The
	// call returns a payment update stream and an error stream.
	SendPayment(ctx context.Context, request SendPaymentRequest) (
		chan PaymentStatus, chan error, error)

	// TrackPayment picks up a previously started payment and returns a
	// payment update stream and an error stream.
	TrackPayment(ctx context.Context, hash lntypes.Hash) (
		chan PaymentStatus, chan error, error)
}

// PaymentStatus describe the state of a payment.
type PaymentStatus struct {
	State    routerrpc.PaymentState
	Preimage lntypes.Preimage
	Fee      lnwire.MilliSatoshi
	Route    *route.Route
}

// SendPaymentRequest defines the payment parameters for a new payment.
type SendPaymentRequest struct {
	Invoice         string
	MaxFee          btcutil.Amount
	MaxCltv         *int32
	OutgoingChannel *uint64
	Timeout         time.Duration
}

// routerClient is a wrapper around the generated routerrpc proxy.
type routerClient struct {
	client       routerrpc.RouterClient
	routerKitMac serializedMacaroon
}

func newRouterClient(conn *grpc.ClientConn,
	routerKitMac serializedMacaroon) *routerClient {

	return &routerClient{
		client:       routerrpc.NewRouterClient(conn),
		routerKitMac: routerKitMac,
	}
}

// SendPayment attempts to route a payment to the final destination. The call
// returns a payment update stream and an error stream.
func (r *routerClient) SendPayment(ctx context.Context,
	request SendPaymentRequest) (chan PaymentStatus, chan error, error) {

	rpcCtx := r.routerKitMac.WithMacaroonAuth(ctx)
	rpcReq := &routerrpc.SendPaymentRequest{
		FeeLimitSat:    int64(request.MaxFee),
		PaymentRequest: request.Invoice,
		TimeoutSeconds: int32(request.Timeout.Seconds()),
	}
	if request.MaxCltv != nil {
		rpcReq.CltvLimit = *request.MaxCltv
	}
	if request.OutgoingChannel != nil {
		rpcReq.OutgoingChanId = *request.OutgoingChannel
	}

	stream, err := r.client.SendPayment(rpcCtx, rpcReq)
	if err != nil {
		return nil, nil, err
	}

	return r.trackPayment(ctx, stream)
}

// TrackPayment picks up a previously started payment and returns a payment
// update stream and an error stream.
func (r *routerClient) TrackPayment(ctx context.Context,
	hash lntypes.Hash) (chan PaymentStatus, chan error, error) {

	ctx = r.routerKitMac.WithMacaroonAuth(ctx)
	stream, err := r.client.TrackPayment(
		ctx, &routerrpc.TrackPaymentRequest{
			PaymentHash: hash[:],
		},
	)
	if err != nil {
		return nil, nil, err
	}

	return r.trackPayment(ctx, stream)
}

// trackPayment takes an update stream from either a SendPayment or a
// TrackPayment rpc call and converts it into distinct update and error streams.
func (r *routerClient) trackPayment(ctx context.Context,
	stream routerrpc.Router_TrackPaymentClient) (chan PaymentStatus,
	chan error, error) {

	statusChan := make(chan PaymentStatus)
	errorChan := make(chan error, 1)
	go func() {
		for {
			rpcStatus, err := stream.Recv()
			if err != nil {
				switch status.Convert(err).Code() {

				// NotFound is only expected as a response to
				// TrackPayment.
				case codes.NotFound:
					err = channeldb.ErrPaymentNotInitiated

				// NotFound is only expected as a response to
				// SendPayment.
				case codes.AlreadyExists:
					err = channeldb.ErrAlreadyPaid
				}

				errorChan <- err
				return
			}

			status, err := unmarshallPaymentStatus(rpcStatus)
			if err != nil {
				errorChan <- err
				return
			}

			select {
			case statusChan <- *status:
			case <-ctx.Done():
				return
			}
		}
	}()

	return statusChan, errorChan, nil
}

// unmarshallPaymentStatus converts an rpc status update to the PaymentStatus
// type that is used throughout the application.
func unmarshallPaymentStatus(rpcStatus *routerrpc.PaymentStatus) (
	*PaymentStatus, error) {

	status := PaymentStatus{
		State: rpcStatus.State,
	}

	if status.State == routerrpc.PaymentState_SUCCEEDED {
		preimage, err := lntypes.MakePreimage(
			rpcStatus.Preimage,
		)
		if err != nil {
			return nil, err
		}
		status.Preimage = preimage

		status.Fee = lnwire.MilliSatoshi(
			rpcStatus.Route.TotalFeesMsat,
		)

		if rpcStatus.Route != nil {
			route, err := unmarshallRoute(rpcStatus.Route)
			if err != nil {
				return nil, err
			}
			status.Route = route
		}
	}

	return &status, nil
}

// unmarshallRoute unmarshalls an rpc route.
func unmarshallRoute(rpcroute *lnrpc.Route) (
	*route.Route, error) {

	hops := make([]*route.Hop, len(rpcroute.Hops))
	for i, hop := range rpcroute.Hops {
		routeHop, err := unmarshallHop(hop)
		if err != nil {
			return nil, err
		}

		hops[i] = routeHop
	}

	// TODO(joostjager): Fetch self node from lnd.
	selfNode := route.Vertex{}

	route, err := route.NewRouteFromHops(
		lnwire.MilliSatoshi(rpcroute.TotalAmtMsat),
		rpcroute.TotalTimeLock,
		selfNode,
		hops,
	)
	if err != nil {
		return nil, err
	}

	return route, nil
}

// unmarshallKnownPubkeyHop unmarshalls an rpc hop.
func unmarshallHop(hop *lnrpc.Hop) (*route.Hop, error) {
	pubKey, err := hex.DecodeString(hop.PubKey)
	if err != nil {
		return nil, fmt.Errorf("cannot decode pubkey %s", hop.PubKey)
	}

	var pubKeyBytes [33]byte
	copy(pubKeyBytes[:], pubKey)

	return &route.Hop{
		OutgoingTimeLock: hop.Expiry,
		AmtToForward:     lnwire.MilliSatoshi(hop.AmtToForwardMsat),
		PubKeyBytes:      pubKeyBytes,
		ChannelID:        hop.ChanId,
	}, nil
}
