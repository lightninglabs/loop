package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/lightningnetwork/lnd/queue"

	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/looprpc"
)

const completedSwapsCount = 5

// swapClientServer implements the grpc service exposed by loopd.
type swapClientServer struct {
	impl *loop.Client
	lnd  *lndclient.LndServices
}

// LoopOut initiates an loop out swap with the given parameters. The call
// returns after the swap has been set up with the swap server. From that point
// onwards, progress can be tracked via the LoopOutStatus stream that is
// returned from Monitor().
func (s *swapClientServer) LoopOut(ctx context.Context,
	in *looprpc.LoopOutRequest) (
	*looprpc.SwapResponse, error) {

	logger.Infof("LoopOut request received")

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

	req := &loop.OutRequest{
		Amount:              btcutil.Amount(in.Amt),
		DestAddr:            sweepAddr,
		MaxMinerFee:         btcutil.Amount(in.MaxMinerFee),
		MaxPrepayAmount:     btcutil.Amount(in.MaxPrepayAmt),
		MaxPrepayRoutingFee: btcutil.Amount(in.MaxPrepayRoutingFee),
		MaxSwapRoutingFee:   btcutil.Amount(in.MaxSwapRoutingFee),
		MaxSwapFee:          btcutil.Amount(in.MaxSwapFee),
		SweepConfTarget:     defaultConfTarget,
	}
	if in.LoopOutChannel != 0 {
		req.LoopOutChannel = &in.LoopOutChannel
	}
	hash, err := s.impl.LoopOut(ctx, req)
	if err != nil {
		logger.Errorf("LoopOut: %v", err)
		return nil, err
	}

	return &looprpc.SwapResponse{
		Id: hash.String(),
	}, nil
}

func (s *swapClientServer) marshallSwap(loopSwap *loop.SwapInfo) (
	*looprpc.SwapStatus, error) {

	var state looprpc.SwapState
	switch loopSwap.State {
	case loopdb.StateInitiated:
		state = looprpc.SwapState_INITIATED
	case loopdb.StatePreimageRevealed:
		state = looprpc.SwapState_PREIMAGE_REVEALED
	case loopdb.StateSuccess:
		state = looprpc.SwapState_SUCCESS
	default:
		// Return less granular status over rpc.
		state = looprpc.SwapState_FAILED
	}

	htlc, err := swap.NewHtlc(
		loopSwap.CltvExpiry, loopSwap.SenderKey, loopSwap.ReceiverKey,
		loopSwap.SwapHash,
	)
	if err != nil {
		return nil, err
	}

	address, err := htlc.Address(s.lnd.ChainParams)
	if err != nil {
		return nil, err
	}

	return &looprpc.SwapStatus{
		Amt:            int64(loopSwap.AmountRequested),
		Id:             loopSwap.SwapHash.String(),
		State:          state,
		InitiationTime: loopSwap.InitiationTime.UnixNano(),
		LastUpdateTime: loopSwap.LastUpdate.UnixNano(),
		HtlcAddress:    address.EncodeAddress(),
		Type:           looprpc.SwapType_LOOP_OUT,
	}, nil
}

// Monitor will return a stream of swap updates for currently active swaps.
func (s *swapClientServer) Monitor(in *looprpc.MonitorRequest,
	server looprpc.SwapClient_MonitorServer) error {

	logger.Infof("Monitor request received")

	send := func(info loop.SwapInfo) error {
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

	var pendingSwaps, completedSwaps []loop.SwapInfo
	for _, swap := range swaps {
		if swap.State.Type() == loopdb.StateTypePending {
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

			swap := queueItem.(loop.SwapInfo)
			if err := send(swap); err != nil {
				return err
			}
		case <-server.Context().Done():
			return nil
		}
	}
}

// GetTerms returns the terms that the server enforces for swaps.
func (s *swapClientServer) GetLoopOutTerms(ctx context.Context,
	req *looprpc.TermsRequest) (*looprpc.TermsResponse, error) {

	logger.Infof("Terms request received")

	terms, err := s.impl.LoopOutTerms(ctx)
	if err != nil {
		logger.Errorf("Terms request: %v", err)
		return nil, err
	}

	return &looprpc.TermsResponse{
		MinSwapAmount: int64(terms.MinSwapAmount),
		MaxSwapAmount: int64(terms.MaxSwapAmount),
		PrepayAmt:     int64(terms.PrepayAmt),
		SwapFeeBase:   int64(terms.SwapFeeBase),
		SwapFeeRate:   int64(terms.SwapFeeRate),
		CltvDelta:     int32(terms.CltvDelta),
	}, nil
}

// GetQuote returns a quote for a swap with the provided parameters.
func (s *swapClientServer) GetLoopOutQuote(ctx context.Context,
	req *looprpc.QuoteRequest) (*looprpc.QuoteResponse, error) {

	quote, err := s.impl.LoopOutQuote(ctx, &loop.LoopOutQuoteRequest{
		Amount:          btcutil.Amount(req.Amt),
		SweepConfTarget: defaultConfTarget,
	})
	if err != nil {
		return nil, err
	}
	return &looprpc.QuoteResponse{
		MinerFee:  int64(quote.MinerFee),
		PrepayAmt: int64(quote.PrepayAmount),
		SwapFee:   int64(quote.SwapFee),
	}, nil
}
