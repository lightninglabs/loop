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

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/lsat"
	"github.com/lightninglabs/loop/server/serverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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

type swapServerClient interface {
	GetLoopOutTerms(ctx context.Context) (
		*LoopOutTerms, error)

	GetLoopOutQuote(ctx context.Context, amt btcutil.Amount, expiry int32,
		swapPublicationDeadline time.Time) (
		*LoopOutQuote, error)

	GetLoopInTerms(ctx context.Context) (
		*LoopInTerms, error)

	GetLoopInQuote(ctx context.Context, amt btcutil.Amount) (
		*LoopInQuote, error)

	NewLoopOutSwap(ctx context.Context,
		swapHash lntypes.Hash, amount btcutil.Amount, expiry int32,
		receiverKey [33]byte,
		swapPublicationDeadline time.Time) (
		*newLoopOutResponse, error)

	PushLoopOutPreimage(ctx context.Context,
		preimage lntypes.Preimage) error

	NewLoopInSwap(ctx context.Context,
		swapHash lntypes.Hash, amount btcutil.Amount,
		senderKey [33]byte, swapInvoice string, lastHop *route.Vertex) (
		*newLoopInResponse, error)

	// SubscribeLoopOutUpdates subscribes to loop out server state.
	SubscribeLoopOutUpdates(ctx context.Context,
		hash lntypes.Hash) (<-chan *ServerUpdate, <-chan error, error)

	// SubscribeLoopInUpdates subscribes to loop in server state.
	SubscribeLoopInUpdates(ctx context.Context,
		hash lntypes.Hash) (<-chan *ServerUpdate, <-chan error, error)
}

type grpcSwapServerClient struct {
	server serverrpc.SwapServerClient
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
		cfg.MaxLsatFee,
	)
	serverConn, err := getSwapServerConn(
		cfg.ServerAddress, cfg.ProxyAddress, cfg.SwapServerNoTLS,
		cfg.TLSPathServer, clientInterceptor,
	)
	if err != nil {
		return nil, err
	}

	server := serverrpc.NewSwapServerClient(serverConn)

	return &grpcSwapServerClient{
		conn:   serverConn,
		server: server,
	}, nil
}

func (s *grpcSwapServerClient) GetLoopOutTerms(ctx context.Context) (
	*LoopOutTerms, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	terms, err := s.server.LoopOutTerms(rpcCtx,
		&serverrpc.ServerLoopOutTermsRequest{
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion,
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
	amt btcutil.Amount, expiry int32, swapPublicationDeadline time.Time) (
	*LoopOutQuote, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	quoteResp, err := s.server.LoopOutQuote(rpcCtx,
		&serverrpc.ServerLoopOutQuoteRequest{
			Amt:                     uint64(amt),
			SwapPublicationDeadline: swapPublicationDeadline.Unix(),
			ProtocolVersion:         loopdb.CurrentRPCProtocolVersion,
			Expiry:                  expiry,
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

func (s *grpcSwapServerClient) GetLoopInTerms(ctx context.Context) (
	*LoopInTerms, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	terms, err := s.server.LoopInTerms(rpcCtx,
		&serverrpc.ServerLoopInTermsRequest{
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion,
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
	amt btcutil.Amount) (*LoopInQuote, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	quoteResp, err := s.server.LoopInQuote(rpcCtx,
		&serverrpc.ServerLoopInQuoteRequest{
			Amt:             uint64(amt),
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion,
		},
	)
	if err != nil {
		return nil, err
	}

	return &LoopInQuote{
		SwapFee:   btcutil.Amount(quoteResp.SwapFee),
		CltvDelta: quoteResp.CltvDelta,
	}, nil
}

func (s *grpcSwapServerClient) NewLoopOutSwap(ctx context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount, expiry int32,
	receiverKey [33]byte, swapPublicationDeadline time.Time) (
	*newLoopOutResponse, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	swapResp, err := s.server.NewLoopOutSwap(rpcCtx,
		&serverrpc.ServerLoopOutRequest{
			SwapHash:                swapHash[:],
			Amt:                     uint64(amount),
			ReceiverKey:             receiverKey[:],
			SwapPublicationDeadline: swapPublicationDeadline.Unix(),
			ProtocolVersion:         loopdb.CurrentRPCProtocolVersion,
			Expiry:                  expiry,
		},
	)
	if err != nil {
		return nil, err
	}

	var senderKey [33]byte
	copy(senderKey[:], swapResp.SenderKey)

	// Validate sender key.
	_, err = btcec.ParsePubKey(senderKey[:], btcec.S256())
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
		&serverrpc.ServerLoopOutPushPreimageRequest{
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion,
			Preimage:        preimage[:],
		},
	)

	return err
}

func (s *grpcSwapServerClient) NewLoopInSwap(ctx context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount, senderKey [33]byte,
	swapInvoice string, lastHop *route.Vertex) (*newLoopInResponse, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()

	req := &serverrpc.ServerLoopInRequest{
		SwapHash:        swapHash[:],
		Amt:             uint64(amount),
		SenderKey:       senderKey[:],
		SwapInvoice:     swapInvoice,
		ProtocolVersion: loopdb.CurrentRPCProtocolVersion,
	}
	if lastHop != nil {
		req.LastHop = lastHop[:]
	}

	swapResp, err := s.server.NewLoopInSwap(rpcCtx, req)
	if err != nil {
		return nil, err
	}

	var receiverKey [33]byte
	copy(receiverKey[:], swapResp.ReceiverKey)

	// Validate receiver key.
	_, err = btcec.ParsePubKey(receiverKey[:], btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("invalid sender key: %v", err)
	}

	return &newLoopInResponse{
		receiverKey:   receiverKey,
		expiry:        swapResp.Expiry,
		serverMessage: swapResp.ServerMessage,
	}, nil
}

// ServerUpdate summarizes an update from the swap server.
type ServerUpdate struct {
	// State is the state that the server has sent us.
	State serverrpc.ServerSwapState

	// Timestamp is the time of the server state update.
	Timestamp time.Time
}

// SubscribeLoopInUpdates subscribes to loop in server state and pipes updates
// into the channel provided.
func (s *grpcSwapServerClient) SubscribeLoopInUpdates(ctx context.Context,
	hash lntypes.Hash) (<-chan *ServerUpdate, <-chan error, error) {

	resp, err := s.server.SubscribeLoopInUpdates(
		ctx, &serverrpc.SubscribeUpdatesRequest{
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion,
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
		ctx, &serverrpc.SubscribeUpdatesRequest{
			ProtocolVersion: loopdb.CurrentRPCProtocolVersion,
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

// getSwapServerConn returns a connection to the swap server. A non-empty
// proxyAddr indicates that a SOCKS proxy found at the address should be used to
// establish the connection.
func getSwapServerConn(address, proxyAddress string, insecure bool,
	tlsPath string, interceptor *lsat.Interceptor) (*grpc.ClientConn, error) {

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
			return tor.Dial(addr, proxyAddress, false)
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
	receiverKey   [33]byte
	expiry        int32
	serverMessage string
}
