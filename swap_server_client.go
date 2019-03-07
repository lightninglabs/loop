package loop

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/lntypes"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type swapServerClient interface {
	GetUnchargeTerms(ctx context.Context) (
		*UnchargeTerms, error)

	NewUnchargeSwap(ctx context.Context,
		swapHash lntypes.Hash, amount btcutil.Amount,
		receiverKey [33]byte) (
		*newUnchargeResponse, error)
}

type grpcSwapServerClient struct {
	server looprpc.SwapServerClient
	conn   *grpc.ClientConn
}

func newSwapServerClient(address string, insecure bool) (*grpcSwapServerClient, error) {
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

func (s *grpcSwapServerClient) GetUnchargeTerms(ctx context.Context) (
	*UnchargeTerms, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, serverRPCTimeout)
	defer rpcCancel()
	quoteResp, err := s.server.UnchargeQuote(rpcCtx,
		&looprpc.ServerUnchargeQuoteRequest{},
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

	return &UnchargeTerms{
		MinSwapAmount:   btcutil.Amount(quoteResp.MinSwapAmount),
		MaxSwapAmount:   btcutil.Amount(quoteResp.MaxSwapAmount),
		PrepayAmt:       btcutil.Amount(quoteResp.PrepayAmt),
		SwapFeeBase:     btcutil.Amount(quoteResp.SwapFeeBase),
		SwapFeeRate:     quoteResp.SwapFeeRate,
		CltvDelta:       quoteResp.CltvDelta,
		SwapPaymentDest: destArray,
	}, nil
}

func (s *grpcSwapServerClient) NewUnchargeSwap(ctx context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount, receiverKey [33]byte) (
	*newUnchargeResponse, error) {

	rpcCtx, rpcCancel := context.WithTimeout(ctx, serverRPCTimeout)
	defer rpcCancel()
	swapResp, err := s.server.NewUnchargeSwap(rpcCtx,
		&looprpc.ServerUnchargeSwapRequest{
			SwapHash:    swapHash[:],
			Amt:         uint64(amount),
			ReceiverKey: receiverKey[:],
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

	return &newUnchargeResponse{
		swapInvoice:   swapResp.SwapInvoice,
		prepayInvoice: swapResp.PrepayInvoice,
		senderKey:     senderKey,
		expiry:        swapResp.Expiry,
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

type newUnchargeResponse struct {
	swapInvoice   string
	prepayInvoice string
	senderKey     [33]byte
	expiry        int32
}
