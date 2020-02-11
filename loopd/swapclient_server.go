package loopd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/routing/route"

	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/looprpc"
)

const (
	completedSwapsCount = 5

	// minConfTarget is the minimum confirmation target we'll allow clients
	// to specify. This is driven by the minimum confirmation target allowed
	// by the backing fee estimator.
	minConfTarget = 2
)

// swapClientServer implements the grpc service exposed by loopd.
type swapClientServer struct {
	impl             *loop.Client
	lnd              *lndclient.LndServices
	swaps            map[lntypes.Hash]loop.SwapInfo
	subscribers      map[int]chan<- interface{}
	statusChan       chan loop.SwapInfo
	nextSubscriberID int
	swapsLock        sync.Mutex
}

// LoopOut initiates an loop out swap with the given parameters. The call
// returns after the swap has been set up with the swap server. From that point
// onwards, progress can be tracked via the LoopOutStatus stream that is
// returned from Monitor().
func (s *swapClientServer) LoopOut(ctx context.Context,
	in *looprpc.LoopOutRequest) (
	*looprpc.SwapResponse, error) {

	log.Infof("Loop out request received")

	sweepConfTarget, err := validateConfTarget(
		in.SweepConfTarget, loop.DefaultSweepConfTarget,
	)
	if err != nil {
		return nil, err
	}

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
		SweepConfTarget:     sweepConfTarget,
		SwapPublicationDeadline: time.Unix(
			int64(in.SwapPublicationDeadline), 0,
		),
	}
	if in.LoopOutChannel != 0 {
		req.LoopOutChannel = &in.LoopOutChannel
	}
	hash, htlc, err := s.impl.LoopOut(ctx, req)
	if err != nil {
		log.Errorf("LoopOut: %v", err)
		return nil, err
	}

	return &looprpc.SwapResponse{
		Id:          hash.String(),
		IdBytes:     hash[:],
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
	case swap.TypeIn:
		swapType = looprpc.SwapType_LOOP_IN
	case swap.TypeOut:
		swapType = looprpc.SwapType_LOOP_OUT
	default:
		return nil, errors.New("unknown swap type")
	}

	return &looprpc.SwapStatus{
		Amt:            int64(loopSwap.AmountRequested),
		Id:             loopSwap.SwapHash.String(),
		IdBytes:        loopSwap.SwapHash[:],
		State:          state,
		InitiationTime: loopSwap.InitiationTime.UnixNano(),
		LastUpdateTime: loopSwap.LastUpdate.UnixNano(),
		HtlcAddress:    loopSwap.HtlcAddress.EncodeAddress(),
		Type:           swapType,
		CostServer:     int64(loopSwap.Cost.Server),
		CostOnchain:    int64(loopSwap.Cost.Onchain),
		CostOffchain:   int64(loopSwap.Cost.Offchain),
	}, nil
}

// Monitor will return a stream of swap updates for currently active swaps.
func (s *swapClientServer) Monitor(in *looprpc.MonitorRequest,
	server looprpc.SwapClient_MonitorServer) error {

	log.Infof("Monitor request received")

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
	s.swapsLock.Lock()

	id := s.nextSubscriberID
	s.nextSubscriberID++
	s.subscribers[id] = queue.ChanIn()

	var pendingSwaps, completedSwaps []loop.SwapInfo
	for _, swap := range s.swaps {
		if swap.State.Type() == loopdb.StateTypePending {
			pendingSwaps = append(pendingSwaps, swap)
		} else {
			completedSwaps = append(completedSwaps, swap)
		}
	}

	s.swapsLock.Unlock()

	defer func() {
		queue.Stop()
		s.swapsLock.Lock()
		delete(s.subscribers, id)
		s.swapsLock.Unlock()
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

// ListSwaps returns a list of all currently known swaps and their current
// status.
func (s *swapClientServer) ListSwaps(_ context.Context,
	_ *looprpc.ListSwapsRequest) (*looprpc.ListSwapsResponse, error) {

	var (
		rpcSwaps = make([]*looprpc.SwapStatus, len(s.swaps))
		idx      = 0
		err      error
	)

	// We can just use the server's in-memory cache as that contains the
	// most up-to-date state including temporary failures which aren't
	// persisted to disk. The swaps field is a map, that's why we need an
	// additional index.
	for _, swp := range s.swaps {
		swp := swp
		rpcSwaps[idx], err = s.marshallSwap(&swp)
		if err != nil {
			return nil, err
		}
		idx++
	}
	return &looprpc.ListSwapsResponse{Swaps: rpcSwaps}, nil
}

// SwapInfo returns all known details about a single swap.
func (s *swapClientServer) SwapInfo(_ context.Context,
	req *looprpc.SwapInfoRequest) (*looprpc.SwapStatus, error) {

	swapHash, err := lntypes.MakeHash(req.Id)
	if err != nil {
		return nil, fmt.Errorf("error parsing swap hash: %v", err)
	}

	// Just return the server's in-memory cache here too as we also want to
	// return temporary failures to the client.
	swp, ok := s.swaps[swapHash]
	if !ok {
		return nil, fmt.Errorf("swap with hash %s not found", req.Id)
	}
	return s.marshallSwap(&swp)
}

// LoopOutTerms returns the terms that the server enforces for loop out swaps.
func (s *swapClientServer) LoopOutTerms(ctx context.Context,
	req *looprpc.TermsRequest) (*looprpc.TermsResponse, error) {

	log.Infof("Loop out terms request received")

	terms, err := s.impl.LoopOutTerms(ctx)
	if err != nil {
		log.Errorf("Terms request: %v", err)
		return nil, err
	}

	return &looprpc.TermsResponse{
		MinSwapAmount: int64(terms.MinSwapAmount),
		MaxSwapAmount: int64(terms.MaxSwapAmount),
	}, nil
}

// LoopOutQuote returns a quote for a loop out swap with the provided
// parameters.
func (s *swapClientServer) LoopOutQuote(ctx context.Context,
	req *looprpc.QuoteRequest) (*looprpc.QuoteResponse, error) {

	confTarget, err := validateConfTarget(
		req.ConfTarget, loop.DefaultSweepConfTarget,
	)
	if err != nil {
		return nil, err
	}
	quote, err := s.impl.LoopOutQuote(ctx, &loop.LoopOutQuoteRequest{
		Amount:          btcutil.Amount(req.Amt),
		SweepConfTarget: confTarget,
		SwapPublicationDeadline: time.Unix(
			int64(req.SwapPublicationDeadline), 0,
		),
	})
	if err != nil {
		return nil, err
	}

	return &looprpc.QuoteResponse{
		MinerFee:        int64(quote.MinerFee),
		PrepayAmt:       int64(quote.PrepayAmount),
		SwapFee:         int64(quote.SwapFee),
		SwapPaymentDest: quote.SwapPaymentDest[:],
		CltvDelta:       quote.CltvDelta,
	}, nil
}

// GetTerms returns the terms that the server enforces for swaps.
func (s *swapClientServer) GetLoopInTerms(ctx context.Context, req *looprpc.TermsRequest) (
	*looprpc.TermsResponse, error) {

	log.Infof("Loop in terms request received")

	terms, err := s.impl.LoopInTerms(ctx)
	if err != nil {
		log.Errorf("Terms request: %v", err)
		return nil, err
	}

	return &looprpc.TermsResponse{
		MinSwapAmount: int64(terms.MinSwapAmount),
		MaxSwapAmount: int64(terms.MaxSwapAmount),
	}, nil
}

// GetQuote returns a quote for a swap with the provided parameters.
func (s *swapClientServer) GetLoopInQuote(ctx context.Context,
	req *looprpc.QuoteRequest) (*looprpc.QuoteResponse, error) {

	log.Infof("Loop in quote request received")

	quote, err := s.impl.LoopInQuote(ctx, &loop.LoopInQuoteRequest{
		Amount:         btcutil.Amount(req.Amt),
		HtlcConfTarget: defaultConfTarget,
		ExternalHtlc:   req.ExternalHtlc,
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

	log.Infof("Loop in request received")

	req := &loop.LoopInRequest{
		Amount:         btcutil.Amount(in.Amt),
		MaxMinerFee:    btcutil.Amount(in.MaxMinerFee),
		MaxSwapFee:     btcutil.Amount(in.MaxSwapFee),
		HtlcConfTarget: defaultConfTarget,
		ExternalHtlc:   in.ExternalHtlc,
	}
	if in.LastHop != nil {
		lastHop, err := route.NewVertexFromBytes(in.LastHop)
		if err != nil {
			return nil, err
		}
		req.LastHop = &lastHop
	}
	hash, htlc, err := s.impl.LoopIn(ctx, req)
	if err != nil {
		log.Errorf("Loop in: %v", err)
		return nil, err
	}

	return &looprpc.SwapResponse{
		Id:          hash.String(),
		IdBytes:     hash[:],
		HtlcAddress: htlc.String(),
	}, nil
}

// GetLsatTokens returns all tokens that are contained in the LSAT token store.
func (s *swapClientServer) GetLsatTokens(ctx context.Context,
	_ *looprpc.TokensRequest) (*looprpc.TokensResponse, error) {

	log.Infof("Get LSAT tokens request received")

	tokens, err := s.impl.LsatStore.AllTokens()
	if err != nil {
		return nil, err
	}

	rpcTokens := make([]*looprpc.LsatToken, len(tokens))
	idx := 0
	for key, token := range tokens {
		macBytes, err := token.BaseMacaroon().MarshalBinary()
		if err != nil {
			return nil, err
		}
		rpcTokens[idx] = &looprpc.LsatToken{
			BaseMacaroon:       macBytes,
			PaymentHash:        token.PaymentHash[:],
			PaymentPreimage:    token.Preimage[:],
			AmountPaidMsat:     int64(token.AmountPaid),
			RoutingFeePaidMsat: int64(token.RoutingFeePaid),
			TimeCreated:        token.TimeCreated.Unix(),
			Expired:            !token.IsValid(),
			StorageName:        key,
		}
		idx++
	}

	return &looprpc.TokensResponse{Tokens: rpcTokens}, nil
}

// processStatusUpdates reads updates on the status channel and processes them.
//
// NOTE: This must run inside a goroutine as it blocks until the main context
// shuts down.
func (s *swapClientServer) processStatusUpdates(mainCtx context.Context) {
	for {
		select {
		// On updates, refresh the server's in-memory state and inform
		// subscribers about the changes.
		case swp := <-s.statusChan:
			s.swapsLock.Lock()
			s.swaps[swp.SwapHash] = swp

			for _, subscriber := range s.subscribers {
				select {
				case subscriber <- swp:
				case <-mainCtx.Done():
					return
				}
			}

			s.swapsLock.Unlock()

		// Server is shutting down.
		case <-mainCtx.Done():
			return
		}
	}
}

// validateConfTarget ensures the given confirmation target is valid. If one
// isn't specified (0 value), then the default target is used.
func validateConfTarget(target, defaultTarget int32) (int32, error) {
	switch {
	// Ensure the target respects our minimum threshold.
	case target < minConfTarget:
		return 0, fmt.Errorf("a confirmation target of at least %v "+
			"must be provided", minConfTarget)

	default:
		return target, nil
	}
}
