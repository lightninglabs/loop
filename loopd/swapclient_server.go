package loopd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/routing/route"
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
	mainCtx          context.Context
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
		HtlcConfirmations:   in.HtlcConfirmations,
		SwapPublicationDeadline: time.Unix(
			int64(in.SwapPublicationDeadline), 0,
		),
		Label: in.Label,
	}

	switch {
	case in.LoopOutChannel != 0 && len(in.OutgoingChanSet) > 0:
		return nil, errors.New("loop_out_channel and outgoing_" +
			"chan_ids are mutually exclusive")

	case in.LoopOutChannel != 0:
		req.OutgoingChanSet = loopdb.ChannelSet{in.LoopOutChannel}

	default:
		req.OutgoingChanSet = in.OutgoingChanSet
	}

	info, err := s.impl.LoopOut(ctx, req)
	if err != nil {
		log.Errorf("LoopOut: %v", err)
		return nil, err
	}

	return &looprpc.SwapResponse{
		Id:               info.SwapHash.String(),
		IdBytes:          info.SwapHash[:],
		HtlcAddress:      info.HtlcAddressP2WSH.String(),
		HtlcAddressP2Wsh: info.HtlcAddressP2WSH.String(),
		ServerMessage:    info.ServerMessage,
	}, nil
}

func (s *swapClientServer) marshallSwap(loopSwap *loop.SwapInfo) (
	*looprpc.SwapStatus, error) {

	var (
		state         looprpc.SwapState
		failureReason = looprpc.FailureReason_FAILURE_REASON_NONE
	)

	// Set our state var for non-failure states. If we get a failure, we
	// will update our failure reason. To remain backwards compatible with
	// previous versions where we squashed all failure reasons to a single
	// failure state, we set a failure reason for all our different failure
	// states, and set our failed state for all of them.
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

	case loopdb.StateFailOffchainPayments:
		failureReason = looprpc.FailureReason_FAILURE_REASON_OFFCHAIN

	case loopdb.StateFailTimeout:
		failureReason = looprpc.FailureReason_FAILURE_REASON_TIMEOUT

	case loopdb.StateFailSweepTimeout:
		failureReason = looprpc.FailureReason_FAILURE_REASON_SWEEP_TIMEOUT

	case loopdb.StateFailInsufficientValue:
		failureReason = looprpc.FailureReason_FAILURE_REASON_INSUFFICIENT_VALUE

	case loopdb.StateFailTemporary:
		failureReason = looprpc.FailureReason_FAILURE_REASON_TEMPORARY

	case loopdb.StateFailIncorrectHtlcAmt:
		failureReason = looprpc.FailureReason_FAILURE_REASON_INCORRECT_AMOUNT

	default:
		return nil, fmt.Errorf("unknown swap state: %v", loopSwap.State)
	}

	// If we have a failure reason, we have a failure state, so should use
	// our catchall failed state.
	if failureReason != looprpc.FailureReason_FAILURE_REASON_NONE {
		state = looprpc.SwapState_FAILED
	}

	var swapType looprpc.SwapType
	var htlcAddress, htlcAddressP2WSH, htlcAddressNP2WSH string

	switch loopSwap.SwapType {
	case swap.TypeIn:
		swapType = looprpc.SwapType_LOOP_IN
		htlcAddressP2WSH = loopSwap.HtlcAddressP2WSH.EncodeAddress()

		if loopSwap.ExternalHtlc {
			htlcAddressNP2WSH = loopSwap.HtlcAddressNP2WSH.EncodeAddress()
			htlcAddress = htlcAddressNP2WSH
		} else {
			htlcAddress = htlcAddressP2WSH
		}

	case swap.TypeOut:
		swapType = looprpc.SwapType_LOOP_OUT
		htlcAddressP2WSH = loopSwap.HtlcAddressP2WSH.EncodeAddress()
		htlcAddress = htlcAddressP2WSH

	default:
		return nil, errors.New("unknown swap type")
	}

	return &looprpc.SwapStatus{
		Amt:               int64(loopSwap.AmountRequested),
		Id:                loopSwap.SwapHash.String(),
		IdBytes:           loopSwap.SwapHash[:],
		State:             state,
		FailureReason:     failureReason,
		InitiationTime:    loopSwap.InitiationTime.UnixNano(),
		LastUpdateTime:    loopSwap.LastUpdate.UnixNano(),
		HtlcAddress:       htlcAddress,
		HtlcAddressP2Wsh:  htlcAddressP2WSH,
		HtlcAddressNp2Wsh: htlcAddressNP2WSH,
		Type:              swapType,
		CostServer:        int64(loopSwap.Cost.Server),
		CostOnchain:       int64(loopSwap.Cost.Onchain),
		CostOffchain:      int64(loopSwap.Cost.Offchain),
		Label:             loopSwap.Label,
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
		s.swapsLock.Lock()
		delete(s.subscribers, id)
		s.swapsLock.Unlock()
		queue.Stop()
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

		// The client cancels the subscription.
		case <-server.Context().Done():
			return nil

		// The server is shutting down.
		case <-s.mainCtx.Done():
			return fmt.Errorf("server is shutting down")
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

	s.swapsLock.Lock()
	defer s.swapsLock.Unlock()

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
	req *looprpc.TermsRequest) (*looprpc.OutTermsResponse, error) {

	log.Infof("Loop out terms request received")

	terms, err := s.impl.LoopOutTerms(ctx)
	if err != nil {
		log.Errorf("Terms request: %v", err)
		return nil, err
	}

	return &looprpc.OutTermsResponse{
		MinSwapAmount: int64(terms.MinSwapAmount),
		MaxSwapAmount: int64(terms.MaxSwapAmount),
		MinCltvDelta:  terms.MinCltvDelta,
		MaxCltvDelta:  terms.MaxCltvDelta,
	}, nil
}

// LoopOutQuote returns a quote for a loop out swap with the provided
// parameters.
func (s *swapClientServer) LoopOutQuote(ctx context.Context,
	req *looprpc.QuoteRequest) (*looprpc.OutQuoteResponse, error) {

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

	return &looprpc.OutQuoteResponse{
		HtlcSweepFeeSat: int64(quote.MinerFee),
		PrepayAmtSat:    int64(quote.PrepayAmount),
		SwapFeeSat:      int64(quote.SwapFee),
		SwapPaymentDest: quote.SwapPaymentDest[:],
	}, nil
}

// GetTerms returns the terms that the server enforces for swaps.
func (s *swapClientServer) GetLoopInTerms(ctx context.Context,
	req *looprpc.TermsRequest) (*looprpc.InTermsResponse, error) {

	log.Infof("Loop in terms request received")

	terms, err := s.impl.LoopInTerms(ctx)
	if err != nil {
		log.Errorf("Terms request: %v", err)
		return nil, err
	}

	return &looprpc.InTermsResponse{
		MinSwapAmount: int64(terms.MinSwapAmount),
		MaxSwapAmount: int64(terms.MaxSwapAmount),
	}, nil
}

// GetQuote returns a quote for a swap with the provided parameters.
func (s *swapClientServer) GetLoopInQuote(ctx context.Context,
	req *looprpc.QuoteRequest) (*looprpc.InQuoteResponse, error) {

	log.Infof("Loop in quote request received")

	htlcConfTarget, err := validateLoopInRequest(
		req.ConfTarget, req.ExternalHtlc,
	)
	if err != nil {
		return nil, err
	}

	quote, err := s.impl.LoopInQuote(ctx, &loop.LoopInQuoteRequest{
		Amount:         btcutil.Amount(req.Amt),
		HtlcConfTarget: htlcConfTarget,
		ExternalHtlc:   req.ExternalHtlc,
	})
	if err != nil {
		return nil, err
	}
	return &looprpc.InQuoteResponse{
		HtlcPublishFeeSat: int64(quote.MinerFee),
		SwapFeeSat:        int64(quote.SwapFee),
	}, nil
}

func (s *swapClientServer) LoopIn(ctx context.Context,
	in *looprpc.LoopInRequest) (
	*looprpc.SwapResponse, error) {

	log.Infof("Loop in request received")

	htlcConfTarget, err := validateLoopInRequest(
		in.HtlcConfTarget, in.ExternalHtlc,
	)
	if err != nil {
		return nil, err
	}

	req := &loop.LoopInRequest{
		Amount:         btcutil.Amount(in.Amt),
		MaxMinerFee:    btcutil.Amount(in.MaxMinerFee),
		MaxSwapFee:     btcutil.Amount(in.MaxSwapFee),
		HtlcConfTarget: htlcConfTarget,
		ExternalHtlc:   in.ExternalHtlc,
		Label:          in.Label,
	}
	if in.LastHop != nil {
		lastHop, err := route.NewVertexFromBytes(in.LastHop)
		if err != nil {
			return nil, err
		}
		req.LastHop = &lastHop
	}
	swapInfo, err := s.impl.LoopIn(ctx, req)
	if err != nil {
		log.Errorf("Loop in: %v", err)
		return nil, err
	}

	response := &looprpc.SwapResponse{
		Id:               swapInfo.SwapHash.String(),
		IdBytes:          swapInfo.SwapHash[:],
		HtlcAddressP2Wsh: swapInfo.HtlcAddressP2WSH.String(),
		ServerMessage:    swapInfo.ServerMessage,
	}

	if req.ExternalHtlc {
		response.HtlcAddressNp2Wsh = swapInfo.HtlcAddressNP2WSH.String()
		response.HtlcAddress = response.HtlcAddressNp2Wsh
	} else {
		response.HtlcAddress = response.HtlcAddressP2Wsh
	}

	return response, nil
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

// GetLiquidityConfig gets our current liquidity manager's config.
func (s *swapClientServer) GetLiquidityConfig(ctx context.Context,
	_ *looprpc.GetLiquidityConfigRequest) (*looprpc.LiquidityConfig,
	error) {

	cfg, err := s.impl.GetLiquidityConfig(ctx)
	switch err {
	case liquidity.ErrNoParameters:
		return &looprpc.LiquidityConfig{}, nil

	case nil:

	default:
		return nil, err
	}

	rpcCfg := &looprpc.LiquidityConfig{
		IncludePrivate: cfg.IncludePrivate,
	}

	rpcCfg.Target, err = rpcTarget(cfg.Target)
	if err != nil {
		return nil, err
	}

	if cfg.Rule != nil {
		rpcCfg.Rule, err = rpcRule(cfg.Rule)
		if err != nil {
			return nil, err
		}
	}

	return rpcCfg, nil
}

// SetLiquidityConfig attempts to set our current liquidity manager's config.
func (s *swapClientServer) SetLiquidityConfig(ctx context.Context,
	in *looprpc.SetLiquidityConfigRequest) (*looprpc.SetLiquidityConfigResponse,
	error) {

	params := liquidity.Parameters{
		IncludePrivate: in.Config.IncludePrivate,
	}

	switch in.Config.Target {
	case looprpc.LiquidityTarget_NONE:
		params.Target = liquidity.TargetNone

	case looprpc.LiquidityTarget_NODE:
		params.Target = liquidity.TargetNode

	case looprpc.LiquidityTarget_PEER:
		params.Target = liquidity.TargetPeer

	case looprpc.LiquidityTarget_CHANNEL:
		params.Target = liquidity.TargetChannel

	default:
		return nil, fmt.Errorf("unknown target: %v", in.Config.Target)
	}

	if in.Config.Rule != nil {
		var err error
		params.Rule, err = rpcToRule(in.Config.Rule)
		if err != nil {
			return nil, err
		}
	}

	if err := s.impl.SetLiquidityParameters(ctx, params); err != nil {
		return nil, err
	}

	return &looprpc.SetLiquidityConfigResponse{}, nil
}

func rpcToRule(rule *looprpc.LiquidityRule) (liquidity.Rule, error) {
	switch rule.Type {
	case looprpc.LiquidityRuleType_UNKNOWN:
		return nil, nil

	case looprpc.LiquidityRuleType_THRESHOLD:
		return liquidity.NewThresholdRule(
			int(rule.MinimumInbound), int(rule.MinimumOutbound),
		), nil

	case looprpc.LiquidityRuleType_EXCLUDE:
		return liquidity.NewExcludeRule(), nil

	default:
		return nil, fmt.Errorf("unknown rule: %T", rule)
	}

}

func rpcRule(rule liquidity.Rule) (*looprpc.LiquidityRule, error) {
	switch r := rule.(type) {
	case *liquidity.ThresholdRule:
		return &looprpc.LiquidityRule{
			Type:            looprpc.LiquidityRuleType_THRESHOLD,
			MinimumInbound:  uint32(r.MinimumInbound),
			MinimumOutbound: uint32(r.MinimumOutbound),
		}, nil

	case *liquidity.ExcludeRule:
		return &looprpc.LiquidityRule{
			Type: looprpc.LiquidityRuleType_EXCLUDE,
		}, nil

	default:
		return nil, fmt.Errorf("unknown rule: %T", rule)
	}
}

func rpcTarget(target liquidity.Target) (looprpc.LiquidityTarget, error) {
	switch target {
	case liquidity.TargetNone:
		return looprpc.LiquidityTarget_NONE, nil

	case liquidity.TargetNode:
		return looprpc.LiquidityTarget_NODE, nil

	case liquidity.TargetPeer:
		return looprpc.LiquidityTarget_PEER, nil

	case liquidity.TargetChannel:
		return looprpc.LiquidityTarget_CHANNEL, nil

	default:
		return looprpc.LiquidityTarget_NONE,
			fmt.Errorf("unknown target: %v", target)
	}
}

// SuggestSwaps provides a list of suggested swaps based on lnd's current
// channel balances and the target and rule set by the liquidity manager.
func (s *swapClientServer) SuggestSwaps(ctx context.Context,
	_ *looprpc.SuggestSwapsRequest) (*looprpc.SuggestSwapsResponse, error) {

	swaps, err := s.impl.SuggestSwaps(ctx)
	if err != nil {
		return nil, err
	}

	rule, err := rpcRule(swaps.Rule)
	if err != nil {
		return nil, err
	}

	target, err := rpcTarget(swaps.Target)
	if err != nil {
		return nil, err
	}

	resp := &looprpc.SwapSuggestion{
		Rule:   rule,
		Target: target,
	}

	for _, swap := range swaps.Suggestions {
		switch s := swap.(type) {
		case *liquidity.LoopOutRecommendation:
			resp.LoopOut = append(resp.LoopOut,
				&looprpc.LoopOutRequest{
					Amt: int64(swap.Amount()),
					OutgoingChanSet: []uint64{
						s.Channel.ToUint64(),
					},
				},
			)

		case *liquidity.LoopInRecommendation:
			resp.LoopIn = append(resp.LoopIn,
				&looprpc.LoopInRequest{
					Amt:     int64(swap.Amount()),
					LastHop: s.LastHop[:],
				},
			)

		default:
			return nil, fmt.Errorf("unknown swap "+
				"recommendation: %T", swap)
		}
	}

	return &looprpc.SuggestSwapsResponse{
		Suggestions: []*looprpc.SwapSuggestion{resp},
	}, nil
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
					s.swapsLock.Unlock()
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
	case target == 0:
		return defaultTarget, nil

	// Ensure the target respects our minimum threshold.
	case target < minConfTarget:
		return 0, fmt.Errorf("a confirmation target of at least %v "+
			"must be provided", minConfTarget)

	default:
		return target, nil
	}
}

// validateLoopInRequest fails if the mutually exclusive conf target and
// external parameters are both set.
func validateLoopInRequest(htlcConfTarget int32, external bool) (int32, error) {
	// If the htlc is going to be externally set, the htlcConfTarget should
	// not be set, because it has no relevance when the htlc is external.
	if external && htlcConfTarget != 0 {
		return 0, errors.New("external and htlc conf target cannot " +
			"both be set")
	}

	// If the htlc is being externally published, we do not need to set a
	// confirmation target.
	if external {
		return 0, nil
	}

	return validateConfTarget(htlcConfTarget, loop.DefaultHtlcConfTarget)
}
