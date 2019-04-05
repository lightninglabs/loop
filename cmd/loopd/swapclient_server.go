package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/lightningnetwork/lnd/queue"

	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/loopdb"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/looprpc"
)

const completedSwapsCount = 5

var (
	errNoMainnet = errors.New("function not available on mainnet")
)

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

	logger.Infof("Loop out request received")

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
		sweepAddr, err = btcutil.DecodeAddress(
			in.Dest, s.lnd.ChainParams,
		)
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
	hash, htlc, err := s.impl.LoopOut(ctx, req)
	if err != nil {
		logger.Errorf("LoopOut: %v", err)
		return nil, err
	}

	return &looprpc.SwapResponse{
		Id:          hash.String(),
		HtlcAddress: htlc.String(),
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
	case loopdb.StateHtlcPublished:
		state = looprpc.SwapState_HTLC_PUBLISHED
	case loopdb.StateInvoiceSettled:
		state = looprpc.SwapState_INVOICE_SETTLED
	case loopdb.StateSuccess:
		state = looprpc.SwapState_SUCCESS
	default:
		// Return less granular status over rpc.
		state = looprpc.SwapState_FAILED
	}

	var swapType looprpc.SwapType
	switch loopSwap.SwapType {
	case loop.TypeIn:
		swapType = looprpc.SwapType_LOOP_IN
	case loop.TypeOut:
		swapType = looprpc.SwapType_LOOP_OUT
	default:
		return nil, errors.New("unknown swap type")
	}

	return &looprpc.SwapStatus{
		Amt:            int64(loopSwap.AmountRequested),
		Id:             loopSwap.SwapHash.String(),
		State:          state,
		InitiationTime: loopSwap.InitiationTime.UnixNano(),
		LastUpdateTime: loopSwap.LastUpdate.UnixNano(),
		HtlcAddress:    loopSwap.HtlcAddress.EncodeAddress(),
		Type:           swapType,
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

// LoopOutTerms returns the terms that the server enforces for loop out swaps.
func (s *swapClientServer) LoopOutTerms(ctx context.Context,
	req *looprpc.TermsRequest) (*looprpc.TermsResponse, error) {

	logger.Infof("Loop out terms request received")

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

// LoopOutQuote returns a quote for a loop out swap with the provided
// parameters.
func (s *swapClientServer) LoopOutQuote(ctx context.Context,
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

// GetTerms returns the terms that the server enforces for swaps.
func (s *swapClientServer) GetLoopInTerms(ctx context.Context, req *looprpc.TermsRequest) (
	*looprpc.TermsResponse, error) {

	logger.Infof("Loop in terms request received")

	if s.lnd.ChainParams.Name == chaincfg.MainNetParams.Name {
		return nil, errNoMainnet
	}

	terms, err := s.impl.LoopInTerms(ctx)
	if err != nil {
		logger.Errorf("Terms request: %v", err)
		return nil, err
	}

	return &looprpc.TermsResponse{
		MinSwapAmount: int64(terms.MinSwapAmount),
		MaxSwapAmount: int64(terms.MaxSwapAmount),
		SwapFeeBase:   int64(terms.SwapFeeBase),
		SwapFeeRate:   int64(terms.SwapFeeRate),
		CltvDelta:     int32(terms.CltvDelta),
	}, nil
}

// GetQuote returns a quote for a swap with the provided parameters.
func (s *swapClientServer) GetLoopInQuote(ctx context.Context,
	req *looprpc.QuoteRequest) (*looprpc.QuoteResponse, error) {

	logger.Infof("Loop in quote request received")

	if s.lnd.ChainParams.Name == chaincfg.MainNetParams.Name {
		return nil, errNoMainnet
	}

	quote, err := s.impl.LoopInQuote(ctx, &loop.LoopInQuoteRequest{
		Amount:         btcutil.Amount(req.Amt),
		HtlcConfTarget: defaultConfTarget,
	})
	if err != nil {
		return nil, err
	}
	return &looprpc.QuoteResponse{
		MinerFee: int64(quote.MinerFee),
		SwapFee:  int64(quote.SwapFee),
	}, nil
}

func (s *swapClientServer) LoopIn(ctx context.Context,
	in *looprpc.LoopInRequest) (
	*looprpc.SwapResponse, error) {

	logger.Infof("Loop in request received")

	if s.lnd.ChainParams.Name == chaincfg.MainNetParams.Name {
		return nil, errNoMainnet
	}

	req := &loop.LoopInRequest{
		Amount:         btcutil.Amount(in.Amt),
		MaxMinerFee:    btcutil.Amount(in.MaxMinerFee),
		MaxSwapFee:     btcutil.Amount(in.MaxSwapFee),
		HtlcConfTarget: defaultConfTarget,
		ExternalHtlc:   in.ExternalHtlc,
	}
	if in.LoopInChannel != 0 {
		req.LoopInChannel = &in.LoopInChannel
	}
	hash, htlc, err := s.impl.LoopIn(ctx, req)
	if err != nil {
		logger.Errorf("Loop in: %v", err)
		return nil, err
	}

	return &looprpc.SwapResponse{
		Id:          hash.String(),
		HtlcAddress: htlc.String(),
	}, nil
}
