package loop

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/lntypes"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type swapServerClient interface {
	GetLoopOutTerms(ctx context.Context) (
		*LoopOutTerms, error)

	GetLoopOutQuote(ctx context.Context, amt btcutil.Amount) (
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

	NewLoopInSwap(ctx context.Context,
		swapHash lntypes.Hash, amount btcutil.Amount,
		senderKey [33]byte, swapInvoice string) (
		*newLoopInResponse, error)
}

type grpcSwapServerClient struct {
	server looprpc.SwapServerClient
	conn   *grpc.ClientConn
}

var _ swapServerClient = (*grpcSwapServerClient)(nil)

func newSwapServerClient(address string,
	insecure bool) (*grpcSwapServerClient, error) {

	serverConn, err := getSwapServerConn(address, insecure)
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

	rpcCtx, rpcCancel := context.WithTimeout(ctx, serverRPCTimeout)
	defer rpcCancel()
	terms, err := s.server.LoopOutTerms(rpcCtx,
		&looprpc.ServerLoopOutTermsRequest{},
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
	amt btcutil.Amount) (*LoopOutQuote, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, serverRPCTimeout)
	defer rpcCancel()
	quoteResp, err := s.server.LoopOutQuote(rpcCtx,
		&looprpc.ServerLoopOutQuoteRequest{
			Amt: uint64(amt),
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

	rpcCtx, rpcCancel := context.WithTimeout(ctx, serverRPCTimeout)
	defer rpcCancel()
	terms, err := s.server.LoopInTerms(rpcCtx,
		&looprpc.ServerLoopInTermsRequest{},
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

	rpcCtx, rpcCancel := context.WithTimeout(ctx, serverRPCTimeout)
	defer rpcCancel()
	quoteResp, err := s.server.LoopInQuote(rpcCtx,
		&looprpc.ServerLoopInQuoteRequest{
			Amt: uint64(amt),
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

	rpcCtx, rpcCancel := context.WithTimeout(ctx, serverRPCTimeout)
	defer rpcCancel()
	swapResp, err := s.server.NewLoopOutSwap(rpcCtx,
		&looprpc.ServerLoopOutRequest{
			SwapHash:                swapHash[:],
			Amt:                     uint64(amount),
			ReceiverKey:             receiverKey[:],
			SwapPublicationDeadline: swapPublicationDeadline.Unix(),
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
	}, nil
}

func (s *grpcSwapServerClient) NewLoopInSwap(ctx context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount, senderKey [33]byte,
	swapInvoice string) (*newLoopInResponse, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, serverRPCTimeout)
	defer rpcCancel()
	swapResp, err := s.server.NewLoopInSwap(rpcCtx,
		&looprpc.ServerLoopInRequest{
			SwapHash:    swapHash[:],
			Amt:         uint64(amount),
			SenderKey:   senderKey[:],
			SwapInvoice: swapInvoice,
		},
	)
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
		receiverKey: receiverKey,
		expiry:      swapResp.Expiry,
	}, nil
}

func (s *grpcSwapServerClient) Close() {
	s.conn.Close()
}

// getSwapServerConn returns a connection to the swap server.
func getSwapServerConn(address string, insecure bool) (*grpc.ClientConn, error) {
	// Create a dial options array.
	opts := []grpc.DialOption{}
	if insecure {
		opts = append(opts, grpc.WithInsecure())
	} else {
		creds := credentials.NewTLS(&tls.Config{})
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v", err)
	}

	return conn, nil
}

type newLoopOutResponse struct {
	swapInvoice   string
	prepayInvoice string
	senderKey     [33]byte
	expiry        int32
}

type newLoopInResponse struct {
	receiverKey [33]byte
	expiry      int32
}
