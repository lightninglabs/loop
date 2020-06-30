package loop

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/lsat"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// protocolVersion defines the version of the protocol that is currently
// supported by the loop client.
const protocolVersion = looprpc.ProtocolVersion_PREIMAGE_PUSH_LOOP_OUT

type swapServerClient interface {
	GetLoopOutTerms(ctx context.Context) (
		*LoopOutTerms, error)

	GetLoopOutQuote(ctx context.Context, amt btcutil.Amount,
		swapPublicationDeadline time.Time) (
		*LoopOutQuote, error)

	GetLoopInTerms(ctx context.Context) (
		*LoopInTerms, error)

	GetLoopInQuote(ctx context.Context, amt btcutil.Amount) (
		*LoopInQuote, error)

	NewLoopOutSwap(ctx context.Context,
		swapHash lntypes.Hash, amount btcutil.Amount,
		receiverKey [33]byte,
		swapPublicationDeadline time.Time) (
		*newLoopOutResponse, error)

	PushLoopOutPreimage(ctx context.Context,
		preimage lntypes.Preimage) error

	NewLoopInSwap(ctx context.Context,
		swapHash lntypes.Hash, amount btcutil.Amount,
		senderKey [33]byte, swapInvoice string, lastHop *route.Vertex) (
		*newLoopInResponse, error)
}

type grpcSwapServerClient struct {
	server looprpc.SwapServerClient
	conn   *grpc.ClientConn
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

	server := looprpc.NewSwapServerClient(serverConn)

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
		&looprpc.ServerLoopOutTermsRequest{
			ProtocolVersion: protocolVersion,
		},
	)
	if err != nil {
		return nil, err
	}

	return &LoopOutTerms{
		MinSwapAmount: btcutil.Amount(terms.MinSwapAmount),
		MaxSwapAmount: btcutil.Amount(terms.MaxSwapAmount),
	}, nil
}

func (s *grpcSwapServerClient) GetLoopOutQuote(ctx context.Context,
	amt btcutil.Amount, swapPublicationDeadline time.Time) (
	*LoopOutQuote, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	quoteResp, err := s.server.LoopOutQuote(rpcCtx,
		&looprpc.ServerLoopOutQuoteRequest{
			Amt:                     uint64(amt),
			SwapPublicationDeadline: swapPublicationDeadline.Unix(),
			ProtocolVersion:         protocolVersion,
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
		CltvDelta:       quoteResp.CltvDelta,
		SwapPaymentDest: destArray,
	}, nil
}

func (s *grpcSwapServerClient) GetLoopInTerms(ctx context.Context) (
	*LoopInTerms, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	terms, err := s.server.LoopInTerms(rpcCtx,
		&looprpc.ServerLoopInTermsRequest{
			ProtocolVersion: protocolVersion,
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
		&looprpc.ServerLoopInQuoteRequest{
			Amt:             uint64(amt),
			ProtocolVersion: protocolVersion,
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
	swapHash lntypes.Hash, amount btcutil.Amount,
	receiverKey [33]byte, swapPublicationDeadline time.Time) (
	*newLoopOutResponse, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, globalCallTimeout)
	defer rpcCancel()
	swapResp, err := s.server.NewLoopOutSwap(rpcCtx,
		&looprpc.ServerLoopOutRequest{
			SwapHash:                swapHash[:],
			Amt:                     uint64(amount),
			ReceiverKey:             receiverKey[:],
			SwapPublicationDeadline: swapPublicationDeadline.Unix(),
			ProtocolVersion:         protocolVersion,
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
		expiry:        swapResp.Expiry,
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
			ProtocolVersion: protocolVersion,
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

	req := &looprpc.ServerLoopInRequest{
		SwapHash:        swapHash[:],
		Amt:             uint64(amount),
		SenderKey:       senderKey[:],
		SwapInvoice:     swapInvoice,
		ProtocolVersion: protocolVersion,
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

func (s *grpcSwapServerClient) Close() {
	s.conn.Close()
}

// getSwapServerConn returns a connection to the swap server. A non-empty
// proxyAddr indicates that a SOCKS proxy found at the address should be used to
// establish the connection.
func getSwapServerConn(address, proxyAddress string, insecure bool,
	tlsPath string, interceptor *lsat.Interceptor) (*grpc.ClientConn, error) {

	// Create a dial options array.
	opts := []grpc.DialOption{grpc.WithUnaryInterceptor(
		interceptor.UnaryInterceptor,
	)}

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

type newLoopOutResponse struct {
	swapInvoice   string
	prepayInvoice string
	senderKey     [33]byte
	expiry        int32
	serverMessage string
}

type newLoopInResponse struct {
	receiverKey   [33]byte
	expiry        int32
	serverMessage string
}
