package loop

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/aperture/lsat"
	"github.com/lightninglabs/loop/loopdb"
	looprpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

var (
	// errServerSubscriptionComplete is returned when our subscription to
	// server updates exits because the server has no more updates to
	// provide us, because its part in the swap is complete.
	errServerSubscriptionComplete = errors.New("server finished serving " +
		"updates")

	// errSubscriptionFailed is returned when our subscription returns with
	// and EOF, indicating that the server restarted, we had an unexpected
	// network failure. Since we do not have restart-recovery, we note that
	// we will not resume our subscription once this error occurs.
	errSubscriptionFailed = errors.New("failed, no further updates will " +
		"be provided")
)

// RoutingPluginType represents the routing plugin type directly.
type RoutingPluginType uint8

const (
	// RoutingPluginNone is recommended when the client shouldn't use any
	// routing plugin.
	RoutingPluginNone RoutingPluginType = 0

	// RoutingPluginLowHigh is recommended when the client should use
	// low-high routing method.
	RoutingPluginLowHigh RoutingPluginType = 1
)

// String pretty prints a RoutingPluginType.
func (r RoutingPluginType) String() string {
	switch r {
	case RoutingPluginLowHigh:
		return "Low/High"

	default:
		return "None"
	}
}

type swapServerClient interface {
	GetLoopOutTerms(ctx context.Context, initiator string) (
		*LoopOutTerms, error)

	GetLoopOutQuote(ctx context.Context, amt btcutil.Amount, expiry int32,
		swapPublicationDeadline time.Time, initiator string) (
		*LoopOutQuote, error)

	GetLoopInTerms(ctx context.Context, initiator string) (
		*LoopInTerms, error)

	GetLoopInQuote(ctx context.Context, amt btcutil.Amount,
		pubKey route.Vertex, lastHop *route.Vertex,
		routeHints [][]zpay32.HopHint,
		initiator string) (*LoopInQuote, error)

	Probe(ctx context.Context, amt btcutil.Amount, target route.Vertex,
		lastHop *route.Vertex, routeHints [][]zpay32.HopHint) error

	NewLoopOutSwap(ctx context.Context,
		swapHash lntypes.Hash, amount btcutil.Amount, expiry int32,
		receiverKey [33]byte, swapPublicationDeadline time.Time,
		initiator string) (*newLoopOutResponse, error)

	PushLoopOutPreimage(ctx context.Context,
		preimage lntypes.Preimage) error

	NewLoopInSwap(ctx context.Context,
		swapHash lntypes.Hash, amount btcutil.Amount, senderScriptKey,
		senderInternalKey [33]byte, swapInvoice, probeInvoice string,
		lastHop *route.Vertex, initiator string) (
		*newLoopInResponse, error)

	// SubscribeLoopOutUpdates subscribes to loop out server state.
	SubscribeLoopOutUpdates(ctx context.Context,
		hash lntypes.Hash) (<-chan *ServerUpdate, <-chan error, error)

	// SubscribeLoopInUpdates subscribes to loop in server state.
	SubscribeLoopInUpdates(ctx context.Context,
		hash lntypes.Hash) (<-chan *ServerUpdate, <-chan error, error)

	// CancelLoopOutSwap cancels a loop out swap.
	CancelLoopOutSwap(ctx context.Context,
		details *outCancelDetails) error

	// RecommendRoutingPlugin asks the server for routing plugin
	// recommendation for off-chain payment(s) of a swap.
	RecommendRoutingPlugin(ctx context.Context, swapHash lntypes.Hash,
		paymentAddr [32]byte) (RoutingPluginType, error)

	// ReportRoutingResult reports a routing result corresponding to a swap.
	ReportRoutingResult(ctx context.Context,
		swapHash lntypes.Hash, paymentAddr [32]byte,
		plugin RoutingPluginType, success bool, attempts int32,
		totalTime int64) error

	// MuSig2SignSweep calls the server to cooperatively sign the MuSig2
	// htlc spend. Returns the server's nonce and partial signature.
	MuSig2SignSweep(ctx context.Context,
		protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
		paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte) (
		[]byte, []byte, error)

	// PushKey sends the client's HTLC internal key associated with the
	// swap to the server.
	PushKey(ctx context.Context,
		protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
		clientInternalPrivateKey [32]byte) error
}

type grpcSwapServerClient struct {
	server looprpc.SwapServerClient
	conn   *grpc.ClientConn

	wg sync.WaitGroup
}

// stop sends the signal for the server's goroutines to shutdown and waits for
// them to complete.
func (s *grpcSwapServerClient) stop() {
	if err := s.conn.Close(); err != nil {
		log.Warnf("could not close connection: %v", err)
	}

	s.wg.Wait()
}

var _ swapServerClient = (*grpcSwapServerClient)(nil)

func newSwapServerClient(cfg *ClientConfig, lsatStore lsat.Store) (
	*grpcSwapServerClient, error) {

	// Create the server connection with the interceptor that will handle
	// the LSAT protocol for us.
	clientInterceptor := lsat.NewInterceptor(
		cfg.Lnd, lsatStore, serverRPCTimeout, cfg.MaxLsatCost,
		cfg.MaxLsatFee, false,
	)
	serverConn, err := getSwapServerConn(
		cfg.ServerAddress, cfg.ProxyAddress, cfg.SwapServerNoTLS,
		cfg.TLSPathServer, clientInterceptor,
	)
	if err != nil {
		return nil, err
	}

	server := looprpc.NewSwapServerClient(serverConn)

	return &grpcSwapServerClient{
		conn:   serverConn,
		server: server,
	}, nil
}

func (s *grpcSwapServerClient) GetLoopOutTerms(ctx context.Context,
	initiator string) (*LoopOutTerms, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	terms, err := s.server.LoopOutTerms(rpcCtx,
		&looprpc.ServerLoopOutTermsRequest{
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
			UserAgent:       UserAgent(initiator),
		},
	)
	if err != nil {
		return nil, err
	}

	return &LoopOutTerms{
		MinSwapAmount: btcutil.Amount(terms.MinSwapAmount),
		MaxSwapAmount: btcutil.Amount(terms.MaxSwapAmount),
		MinCltvDelta:  terms.MinCltvDelta,
		MaxCltvDelta:  terms.MaxCltvDelta,
	}, nil
}

func (s *grpcSwapServerClient) GetLoopOutQuote(ctx context.Context,
	amt btcutil.Amount, expiry int32, swapPublicationDeadline time.Time,
	initiator string) (*LoopOutQuote, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	quoteResp, err := s.server.LoopOutQuote(rpcCtx,
		&looprpc.ServerLoopOutQuoteRequest{
			Amt:                     uint64(amt),
			SwapPublicationDeadline: swapPublicationDeadline.Unix(),
			ProtocolVersion:         loopdb.CurrentRPCProtocolVersion(),
			Expiry:                  expiry,
			UserAgent:               UserAgent(initiator),
		},
	)
	if err != nil {
		return nil, err
	}

	dest, err := hex.DecodeString(quoteResp.SwapPaymentDest)
	if err != nil {
		return nil, err
	}
	if len(dest) != 33 {
		return nil, errors.New("invalid payment dest")
	}
	var destArray [33]byte
	copy(destArray[:], dest)

	return &LoopOutQuote{
		PrepayAmount:    btcutil.Amount(quoteResp.PrepayAmt),
		SwapFee:         btcutil.Amount(quoteResp.SwapFee),
		SwapPaymentDest: destArray,
	}, nil
}

func (s *grpcSwapServerClient) GetLoopInTerms(ctx context.Context,
	initiator string) (*LoopInTerms, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	terms, err := s.server.LoopInTerms(rpcCtx,
		&looprpc.ServerLoopInTermsRequest{
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
			UserAgent:       UserAgent(initiator),
		},
	)
	if err != nil {
		return nil, err
	}

	return &LoopInTerms{
		MinSwapAmount: btcutil.Amount(terms.MinSwapAmount),
		MaxSwapAmount: btcutil.Amount(terms.MaxSwapAmount),
	}, nil
}

func (s *grpcSwapServerClient) GetLoopInQuote(ctx context.Context,
	amt btcutil.Amount, pubKey route.Vertex, lastHop *route.Vertex,
	routeHints [][]zpay32.HopHint, initiator string) (*LoopInQuote, error) {

	err := s.Probe(ctx, amt, pubKey, lastHop, routeHints)
	if err != nil && status.Code(err) != codes.Unavailable {
		log.Warnf("Server probe error: %v", err)
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()

	req := &looprpc.ServerLoopInQuoteRequest{
		Amt:             uint64(amt),
		ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
		Pubkey:          pubKey[:],
		UserAgent:       UserAgent(initiator),
	}

	if lastHop != nil {
		req.LastHop = lastHop[:]
	}

	if routeHints != nil {
		rh, err := marshallRouteHints(routeHints)
		if err != nil {
			return nil, err
		}
		req.RouteHints = rh
	}

	quoteResp, err := s.server.LoopInQuote(rpcCtx, req)
	if err != nil {
		return nil, err
	}

	return &LoopInQuote{
		SwapFee:   btcutil.Amount(quoteResp.SwapFee),
		CltvDelta: quoteResp.CltvDelta,
	}, nil
}

// marshallRouteHints marshalls a list of route hints.
func marshallRouteHints(routeHints [][]zpay32.HopHint) (
	[]*looprpc.RouteHint, error) {

	rpcRouteHints := make([]*looprpc.RouteHint, 0, len(routeHints))
	for _, routeHint := range routeHints {
		rpcRouteHint := make(
			[]*looprpc.HopHint, 0, len(routeHint),
		)
		for _, hint := range routeHint {
			rpcHint, err := marshallHopHint(hint)
			if err != nil {
				return nil, err
			}

			rpcRouteHint = append(rpcRouteHint, rpcHint)
		}
		rpcRouteHints = append(rpcRouteHints, &looprpc.RouteHint{
			HopHints: rpcRouteHint,
		})
	}

	return rpcRouteHints, nil
}

// marshallHopHint marshalls a single hop hint.
func marshallHopHint(hint zpay32.HopHint) (*looprpc.HopHint, error) {
	nodeID, err := route.NewVertexFromBytes(
		hint.NodeID.SerializeCompressed(),
	)
	if err != nil {
		return nil, err
	}

	return &looprpc.HopHint{
		ChanId:                    hint.ChannelID,
		CltvExpiryDelta:           uint32(hint.CLTVExpiryDelta),
		FeeBaseMsat:               hint.FeeBaseMSat,
		FeeProportionalMillionths: hint.FeeProportionalMillionths,
		NodeId:                    nodeID.String(),
	}, nil
}

func (s *grpcSwapServerClient) Probe(ctx context.Context, amt btcutil.Amount,
	target route.Vertex, lastHop *route.Vertex,
	routeHints [][]zpay32.HopHint) error {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, probeTimeout)
	defer rpcCancel()

	rpcRouteHints, err := marshallRouteHints(routeHints)
	if err != nil {
		return err
	}

	req := &looprpc.ServerProbeRequest{
		Amt:             uint64(amt),
		Target:          target[:],
		ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
		RouteHints:      rpcRouteHints,
	}

	if lastHop != nil {
		req.LastHop = lastHop[:]
	}

	_, err = s.server.Probe(rpcCtx, req)
	return err
}

func (s *grpcSwapServerClient) NewLoopOutSwap(ctx context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount, expiry int32,
	receiverKey [33]byte, swapPublicationDeadline time.Time,
	initiator string) (*newLoopOutResponse, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	swapResp, err := s.server.NewLoopOutSwap(rpcCtx,
		&looprpc.ServerLoopOutRequest{
			SwapHash:                swapHash[:],
			Amt:                     uint64(amount),
			ReceiverKey:             receiverKey[:],
			SwapPublicationDeadline: swapPublicationDeadline.Unix(),
			ProtocolVersion:         loopdb.CurrentRPCProtocolVersion(),
			Expiry:                  expiry,
			UserAgent:               UserAgent(initiator),
		},
	)
	if err != nil {
		return nil, err
	}

	var senderKey [33]byte
	copy(senderKey[:], swapResp.SenderKey)

	// Validate sender key.
	_, err = btcec.ParsePubKey(senderKey[:])
	if err != nil {
		return nil, fmt.Errorf("invalid sender key: %v", err)
	}

	return &newLoopOutResponse{
		swapInvoice:   swapResp.SwapInvoice,
		prepayInvoice: swapResp.PrepayInvoice,
		senderKey:     senderKey,
		serverMessage: swapResp.ServerMessage,
	}, nil
}

// PushLoopOutPreimage pushes a preimage to the server.
func (s *grpcSwapServerClient) PushLoopOutPreimage(ctx context.Context,
	preimage lntypes.Preimage) error {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()

	_, err := s.server.LoopOutPushPreimage(rpcCtx,
		&looprpc.ServerLoopOutPushPreimageRequest{
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
			Preimage:        preimage[:],
		},
	)

	return err
}

func (s *grpcSwapServerClient) NewLoopInSwap(ctx context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount, senderScriptKey,
	senderInternalKey [33]byte, swapInvoice, probeInvoice string,
	lastHop *route.Vertex, initiator string) (*newLoopInResponse, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()

	req := &looprpc.ServerLoopInRequest{
		SwapHash:        swapHash[:],
		Amt:             uint64(amount),
		SenderKey:       senderScriptKey[:],
		SwapInvoice:     swapInvoice,
		ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
		ProbeInvoice:    probeInvoice,
		UserAgent:       UserAgent(initiator),
	}
	if lastHop != nil {
		req.LastHop = lastHop[:]
	}

	// Set the client's internal key if this is a MuSig2 swap.
	if loopdb.CurrentProtocolVersion() >= loopdb.ProtocolVersionMuSig2 {
		req.SenderInternalPubkey = senderInternalKey[:]
	}

	swapResp, err := s.server.NewLoopInSwap(rpcCtx, req)
	if err != nil {
		return nil, err
	}

	var receiverKey, receiverInternalKey [33]byte
	copy(receiverKey[:], swapResp.ReceiverKey)
	copy(receiverInternalKey[:], swapResp.ReceiverInternalPubkey)

	// Validate receiver key.
	_, err = btcec.ParsePubKey(receiverKey[:])
	if err != nil {
		return nil, fmt.Errorf("invalid sender key: %v", err)
	}

	return &newLoopInResponse{
		receiverKey:         receiverKey,
		receiverInternalKey: receiverInternalKey,
		expiry:              swapResp.Expiry,
		serverMessage:       swapResp.ServerMessage,
	}, nil
}

// ServerUpdate summarizes an update from the swap server.
type ServerUpdate struct {
	// State is the state that the server has sent us.
	State looprpc.ServerSwapState

	// Timestamp is the time of the server state update.
	Timestamp time.Time
}

// SubscribeLoopInUpdates subscribes to loop in server state and pipes updates
// into the channel provided.
func (s *grpcSwapServerClient) SubscribeLoopInUpdates(ctx context.Context,
	hash lntypes.Hash) (<-chan *ServerUpdate, <-chan error, error) {

	resp, err := s.server.SubscribeLoopInUpdates(
		ctx, &looprpc.SubscribeUpdatesRequest{
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
			SwapHash:        hash[:],
		},
	)
	if err != nil {
		return nil, nil, err
	}

	receive := func() (*ServerUpdate, error) {
		response, err := resp.Recv()
		if err != nil {
			return nil, err
		}

		return &ServerUpdate{
			State:     response.State,
			Timestamp: time.Unix(0, response.TimestampNs),
		}, nil
	}

	updateChan, errChan := s.makeServerUpdate(ctx, receive)
	return updateChan, errChan, nil
}

// SubscribeLoopOutUpdates subscribes to loop out server state and pipes updates
// into the channel provided.
func (s *grpcSwapServerClient) SubscribeLoopOutUpdates(ctx context.Context,
	hash lntypes.Hash) (<-chan *ServerUpdate, <-chan error, error) {

	resp, err := s.server.SubscribeLoopOutUpdates(
		ctx, &looprpc.SubscribeUpdatesRequest{
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
			SwapHash:        hash[:],
		},
	)
	if err != nil {
		return nil, nil, err
	}

	receive := func() (*ServerUpdate, error) {
		response, err := resp.Recv()
		if err != nil {
			return nil, err
		}

		return &ServerUpdate{
			State:     response.State,
			Timestamp: time.Unix(0, response.TimestampNs),
		}, nil
	}

	updateChan, errChan := s.makeServerUpdate(ctx, receive)
	return updateChan, errChan, nil
}

// makeServerUpdate takes a stream receive function and a channel that it
// should pipe updates into. It sends events into the updates channel until
// the client cancels, server client shuts down or the subscription is cancelled
// server side.
func (s *grpcSwapServerClient) makeServerUpdate(ctx context.Context,
	receive func() (*ServerUpdate, error)) (<-chan *ServerUpdate,
	<-chan error) {

	// We will return exactly one error from this function so we buffer
	// our error channel so that the function exit is not dependent on
	// the error being read.
	errChan := make(chan error, 1)
	updateChan := make(chan *ServerUpdate)

	// Create a goroutine that will pipe updates in to our updates channel.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		for {
			// Try to receive from our stream. If there are no items
			// to consume, this call will block. If our stream is
			// cancelled by the server we will receive an error.
			response, err := receive()
			switch err {
			// If we get a nil error, we proceed with to delivering
			// the update we have just received.
			case nil:

			// If we get an EOF error, the server is finished
			// sending us updates, so we return with a non-nil
			// a subscription complete error to inform the caller
			// that they will no longer receive updates.
			case io.EOF:
				errChan <- errServerSubscriptionComplete
				return

			// If we receive a non-nil error, we exit.
			default:
				// If we get a transport is closing error, we
				// send a server restarting error so that the
				// caller is informed that we will not get
				// any more updates from the server (since we
				// don't have retry logic yet).
				if isErrConClosing(err) {
					errChan <- errSubscriptionFailed
				} else {
					errChan <- err
				}

				return
			}

			select {
			// Try to send our update to the update channel.
			case updateChan <- response:

			// If the client cancels their context, we exit with
			// no error.
			case <-ctx.Done():
				errChan <- nil
				return
			}
		}
	}()

	return updateChan, errChan
}

// paymentType is an enum representing different types of off-chain payments
// made by a swap.
type paymentType uint8

const (
	// paymentTypePrepay indicates that we could not route the prepay.
	paymentTypePrepay paymentType = iota

	// paymentTypeInvoice indicates that we could not route the swap
	// invoice.
	paymentTypeInvoice
)

// routeCancelMetadata contains cancelation information for swaps that are
// canceled because the client could not route off-chain to the server.
type routeCancelMetadata struct {
	// paymentType is the type of payment that failed.
	paymentType paymentType

	// attempts is the set of htlc attempts made by the client, reporting
	// the distance from the invoice's destination node that a failure
	// occurred.
	attempts []uint32

	// failureReason is the reason that the payment failed.
	failureReason lnrpc.PaymentFailureReason
}

// outCancelDetails contains the informaton required to cancel a loop out swap.
type outCancelDetails struct {
	// Hash is the swap's hash.
	hash lntypes.Hash

	// paymentAddr is the payment address for the swap's invoice.
	paymentAddr [32]byte

	// metadata contains additional information about the swap.
	metadata routeCancelMetadata
}

// CancelLoopOutSwap sends an instruction to the server to cancel a loop out
// swap.
func (s *grpcSwapServerClient) CancelLoopOutSwap(ctx context.Context,
	details *outCancelDetails) error {

	req := &looprpc.CancelLoopOutSwapRequest{
		ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
		SwapHash:        details.hash[:],
		PaymentAddress:  details.paymentAddr[:],
	}

	var err error
	req.CancelInfo, err = rpcRouteCancel(details)
	if err != nil {
		return err
	}

	_, err = s.server.CancelLoopOutSwap(ctx, req)
	return err
}

// RecommendRoutingPlugin asks the server for routing plugin recommendation for
// off-chain payment(s) of a swap.
func (s *grpcSwapServerClient) RecommendRoutingPlugin(ctx context.Context,
	swapHash lntypes.Hash, paymentAddr [32]byte) (RoutingPluginType, error) {

	req := &looprpc.RecommendRoutingPluginReq{
		ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
		SwapHash:        swapHash[:],
		PaymentAddress:  paymentAddr[:],
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()

	res, err := s.server.RecommendRoutingPlugin(rpcCtx, req)
	if err != nil {
		return RoutingPluginNone, err
	}

	var plugin RoutingPluginType
	switch res.Plugin {
	case looprpc.RoutingPlugin_NONE:
		plugin = RoutingPluginNone

	case looprpc.RoutingPlugin_LOW_HIGH:
		plugin = RoutingPluginLowHigh

	default:
		log.Warnf("Recommended routing plugin is unknown: %v", plugin)
		plugin = RoutingPluginNone
	}

	return plugin, nil
}

// ReportRoutingResult reports a routing result corresponding to a swap.
func (s *grpcSwapServerClient) ReportRoutingResult(ctx context.Context,
	swapHash lntypes.Hash, paymentAddr [32]byte, plugin RoutingPluginType,
	success bool, attempts int32, totalTime int64) error {

	var rpcRoutingPlugin looprpc.RoutingPlugin
	switch plugin {
	case RoutingPluginLowHigh:
		rpcRoutingPlugin = looprpc.RoutingPlugin_LOW_HIGH

	default:
		rpcRoutingPlugin = looprpc.RoutingPlugin_NONE
	}

	req := &looprpc.ReportRoutingResultReq{
		ProtocolVersion: loopdb.CurrentRPCProtocolVersion(),
		SwapHash:        swapHash[:],
		PaymentAddress:  paymentAddr[:],
		Plugin:          rpcRoutingPlugin,
		Success:         success,
		Attempts:        attempts,
		TotalTime:       totalTime,
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()

	_, err := s.server.ReportRoutingResult(rpcCtx, req)
	return err
}

// MuSig2SignSweep calls the server to cooperatively sign the MuSig2 htlc
// spend. Returns the server's nonce and partial signature.
func (s *grpcSwapServerClient) MuSig2SignSweep(ctx context.Context,
	protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
	paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte) (
	[]byte, []byte, error) {

	req := &looprpc.MuSig2SignSweepReq{
		ProtocolVersion: looprpc.ProtocolVersion(protocolVersion),
		SwapHash:        swapHash[:],
		PaymentAddress:  paymentAddr[:],
		Nonce:           nonce,
		SweepTxPsbt:     sweepTxPsbt,
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()

	res, err := s.server.MuSig2SignSweep(rpcCtx, req)
	if err != nil {
		return nil, nil, err
	}

	return res.Nonce, res.PartialSignature, nil
}

// PushKey sends the client's HTLC internal key associated with the swap to
// the server.
func (s *grpcSwapServerClient) PushKey(ctx context.Context,
	protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
	clientInternalPrivateKey [32]byte) error {

	req := &looprpc.ServerPushKeyReq{
		ProtocolVersion: looprpc.ProtocolVersion(protocolVersion),
		SwapHash:        swapHash[:],
		InternalPrivkey: clientInternalPrivateKey[:],
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()

	_, err := s.server.PushKey(rpcCtx, req)
	return err
}

func rpcRouteCancel(details *outCancelDetails) (
	*looprpc.CancelLoopOutSwapRequest_RouteCancel, error) {

	attempts := make(
		[]*looprpc.HtlcAttempt, len(details.metadata.attempts),
	)
	for i, remaining := range details.metadata.attempts {
		attempts[i] = &looprpc.HtlcAttempt{
			RemainingHops: remaining,
		}
	}

	resp := &looprpc.CancelLoopOutSwapRequest_RouteCancel{
		RouteCancel: &looprpc.RouteCancel{
			Attempts: attempts,
			// We can cast our lnd failure reason to a loop payment
			// failure reason because these values are copied 1:1
			// from lnd.
			Failure: looprpc.PaymentFailureReason(
				details.metadata.failureReason,
			),
		},
	}

	switch details.metadata.paymentType {
	case paymentTypePrepay:
		resp.RouteCancel.RouteType = looprpc.RoutePaymentType_PREPAY_ROUTE

	case paymentTypeInvoice:
		resp.RouteCancel.RouteType = looprpc.RoutePaymentType_INVOICE_ROUTE

	default:
		return nil, fmt.Errorf("unknown payment type: %v",
			details.metadata.paymentType)
	}

	return resp, nil
}

// getSwapServerConn returns a connection to the swap server. A non-empty
// proxyAddr indicates that a SOCKS proxy found at the address should be used to
// establish the connection.
func getSwapServerConn(address, proxyAddress string, insecure bool,
	tlsPath string, interceptor *lsat.ClientInterceptor) (*grpc.ClientConn,
	error) {

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithUnaryInterceptor(
			interceptor.UnaryInterceptor,
		),
		grpc.WithStreamInterceptor(
			interceptor.StreamInterceptor,
		),
	}

	// There are three options to connect to a swap server, either insecure,
	// using a self-signed certificate or with a certificate signed by a
	// public CA.
	switch {
	case insecure:
		opts = append(opts, grpc.WithInsecure())

	case tlsPath != "":
		// Load the specified TLS certificate and build
		// transport credentials
		creds, err := credentials.NewClientTLSFromFile(tlsPath, "")
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))

	default:
		creds := credentials.NewTLS(&tls.Config{})
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	// If a SOCKS proxy address was specified, then we should dial through
	// it.
	if proxyAddress != "" {
		log.Infof("Proxying connection to %v over Tor SOCKS proxy %v",
			address, proxyAddress)
		torDialer := func(_ context.Context, addr string) (net.Conn, error) {
			return tor.Dial(
				addr, proxyAddress, false, false,
				tor.DefaultConnTimeout,
			)
		}
		opts = append(opts, grpc.WithContextDialer(torDialer))
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}

// isErrConClosing identifies whether we have received a "transport is closing"
// error from a grpc stream, indicating that the server has shutdown. We need
// to string match this error because ErrConnClosing is part of an internal
// grpc package, so cannot be used directly.
func isErrConClosing(err error) bool {
	if err == nil {
		return false
	}

	return strings.Contains(err.Error(), "transport is closing")
}

type newLoopOutResponse struct {
	swapInvoice   string
	prepayInvoice string
	senderKey     [33]byte
	serverMessage string
}

type newLoopInResponse struct {
	receiverKey         [33]byte
	receiverInternalKey [33]byte
	expiry              int32
	serverMessage       string
}
