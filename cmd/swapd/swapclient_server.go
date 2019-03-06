package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/lightningnetwork/lnd/queue"

	"github.com/lightninglabs/nautilus/lndclient"
	"github.com/lightninglabs/nautilus/utils"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/nautilus/client"
	clientrpc "github.com/lightninglabs/nautilus/cmd/swapd/rpc"
)

const completedSwapsCount = 5

// swapClientServer implements the grpc service exposed by swapd.
type swapClientServer struct {
	impl *client.Client
	lnd  *lndclient.LndServices
}

// Uncharge initiates an uncharge swap with the given parameters. The call
// returns after the swap has been set up with the swap server. From that point
// onwards, progress can be tracked via the UnchargeStatus stream that is
// returned from Monitor().
func (s *swapClientServer) Uncharge(ctx context.Context,
	in *clientrpc.UnchargeRequest) (
	*clientrpc.SwapResponse, error) {

	logger.Infof("Uncharge request received")

	var sweepAddr btcutil.Address
	if in.Dest == "" {
		// Generate sweep address if none specified.
		var err error
		sweepAddr, err = s.lnd.WalletKit.NextAddr(context.Background())
		if err != nil {
			return nil, fmt.Errorf("NextAddr error: %v", err)
		}
	} else {
		var err error
		sweepAddr, err = btcutil.DecodeAddress(in.Dest, nil)
		if err != nil {
			return nil, fmt.Errorf("decode address: %v", err)
		}
	}

	req := &client.UnchargeRequest{
		Amount:              btcutil.Amount(in.Amt),
		DestAddr:            sweepAddr,
		MaxMinerFee:         btcutil.Amount(in.MaxMinerFee),
		MaxPrepayAmount:     btcutil.Amount(in.MaxPrepayAmt),
		MaxPrepayRoutingFee: btcutil.Amount(in.MaxPrepayRoutingFee),
		MaxSwapRoutingFee:   btcutil.Amount(in.MaxSwapRoutingFee),
		MaxSwapFee:          btcutil.Amount(in.MaxSwapFee),
		SweepConfTarget:     defaultConfTarget,
	}
	if in.UnchargeChannel != 0 {
		req.UnchargeChannel = &in.UnchargeChannel
	}
	hash, err := s.impl.Uncharge(ctx, req)
	if err != nil {
		logger.Errorf("Uncharge: %v", err)
		return nil, err
	}

	return &clientrpc.SwapResponse{
		Id: hash.String(),
	}, nil
}

func (s *swapClientServer) marshallSwap(swap *client.SwapInfo) (
	*clientrpc.SwapStatus, error) {

	var state clientrpc.SwapState
	switch swap.State {
	case client.StateInitiated:
		state = clientrpc.SwapState_INITIATED
	case client.StatePreimageRevealed:
		state = clientrpc.SwapState_PREIMAGE_REVEALED
	case client.StateSuccess:
		state = clientrpc.SwapState_SUCCESS
	default:
		// Return less granular status over rpc.
		state = clientrpc.SwapState_FAILED
	}

	htlc, err := utils.NewHtlc(swap.CltvExpiry, swap.SenderKey,
		swap.ReceiverKey, swap.SwapHash,
	)
	if err != nil {
		return nil, err
	}

	address, err := htlc.Address(s.lnd.ChainParams)
	if err != nil {
		return nil, err
	}

	return &clientrpc.SwapStatus{
		Amt:            int64(swap.AmountRequested),
		Id:             swap.SwapHash.String(),
		State:          state,
		InitiationTime: swap.InitiationTime.UnixNano(),
		LastUpdateTime: swap.LastUpdate.UnixNano(),
		HtlcAddress:    address.EncodeAddress(),
		Type:           clientrpc.SwapType_UNCHARGE,
	}, nil
}

// Monitor will return a stream of swap updates for currently active swaps.
func (s *swapClientServer) Monitor(in *clientrpc.MonitorRequest,
	server clientrpc.SwapClient_MonitorServer) error {

	logger.Infof("Monitor request received")

	send := func(info client.SwapInfo) error {
		rpcSwap, err := s.marshallSwap(&info)
		if err != nil {
			return err
		}

		return server.Send(rpcSwap)
	}

	// Start a notification queue for this subscriber.
	queue := queue.NewConcurrentQueue(20)
	queue.Start()

	// Add this subscriber to the global subscriber list. Also create a
	// snapshot of all pending and completed swaps within the lock, to
	// prevent subscribers from receiving duplicate updates.
	swapsLock.Lock()

	id := nextSubscriberID
	nextSubscriberID++
	subscribers[id] = queue.ChanIn()

	var pendingSwaps, completedSwaps []client.SwapInfo
	for _, swap := range swaps {
		if swap.State.Type() == client.StateTypePending {
			pendingSwaps = append(pendingSwaps, swap)
		} else {
			completedSwaps = append(completedSwaps, swap)
		}
	}

	swapsLock.Unlock()

	defer func() {
		queue.Stop()
		swapsLock.Lock()
		delete(subscribers, id)
		swapsLock.Unlock()
	}()

	// Sort completed swaps new to old.
	sort.Slice(completedSwaps, func(i, j int) bool {
		return completedSwaps[i].LastUpdate.After(
			completedSwaps[j].LastUpdate,
		)
	})

	// Discard all but top x latest.
	if len(completedSwaps) > completedSwapsCount {
		completedSwaps = completedSwaps[:completedSwapsCount]
	}

	// Concatenate both sets.
	filteredSwaps := append(pendingSwaps, completedSwaps...)

	// Sort again, but this time old to new.
	sort.Slice(filteredSwaps, func(i, j int) bool {
		return filteredSwaps[i].LastUpdate.Before(
			filteredSwaps[j].LastUpdate,
		)
	})

	// Return swaps to caller.
	for _, swap := range filteredSwaps {
		if err := send(swap); err != nil {
			return err
		}
	}

	// As long as the client is connected, keep passing through swap
	// updates.
	for {
		select {
		case queueItem, ok := <-queue.ChanOut():
			if !ok {
				return nil
			}

			swap := queueItem.(client.SwapInfo)
			if err := send(swap); err != nil {
				return err
			}
		case <-server.Context().Done():
			return nil
		}
	}
}

// GetTerms returns the terms that the server enforces for swaps.
func (s *swapClientServer) GetUnchargeTerms(ctx context.Context, req *clientrpc.TermsRequest) (
	*clientrpc.TermsResponse, error) {

	logger.Infof("Terms request received")

	terms, err := s.impl.UnchargeTerms(ctx)
	if err != nil {
		logger.Errorf("Terms request: %v", err)
		return nil, err
	}

	return &clientrpc.TermsResponse{
		MinSwapAmount: int64(terms.MinSwapAmount),
		MaxSwapAmount: int64(terms.MaxSwapAmount),
		PrepayAmt:     int64(terms.PrepayAmt),
		SwapFeeBase:   int64(terms.SwapFeeBase),
		SwapFeeRate:   int64(terms.SwapFeeRate),
		CltvDelta:     int32(terms.CltvDelta),
	}, nil
}

// GetQuote returns a quote for a swap with the provided parameters.
func (s *swapClientServer) GetUnchargeQuote(ctx context.Context,
	req *clientrpc.QuoteRequest) (*clientrpc.QuoteResponse, error) {

	quote, err := s.impl.UnchargeQuote(ctx, &client.UnchargeQuoteRequest{
		Amount:          btcutil.Amount(req.Amt),
		SweepConfTarget: defaultConfTarget,
	})
	if err != nil {
		return nil, err
	}
	return &clientrpc.QuoteResponse{
		MinerFee:  int64(quote.MinerFee),
		PrepayAmt: int64(quote.PrepayAmount),
		SwapFee:   int64(quote.SwapFee),
	}, nil
}
