package liquidity

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Compile-time assertion that loopInBuilder satisfies the swapBuilder
// interface.
var _ swapBuilder = (*loopInBuilder)(nil)

func newLoopInBuilder(cfg *Config) *loopInBuilder {
	return &loopInBuilder{
		cfg: cfg,
	}
}

type loopInBuilder struct {
	// cfg contains all the external functionality we require to create
	// swaps.
	cfg *Config
}

// swapType returns the swap type that the builder is responsible for creating.
func (b *loopInBuilder) swapType() swap.Type {
	return swap.TypeIn
}

// maySwap checks whether we can currently execute a swap, examining the
// current on-chain fee conditions against relevant to our swap type against
// our fee restrictions.
//
// For loop in, we cannot check any upfront costs because we do not know how
// many inputs will be used for our on-chain htlc before it is made, so we can't
// make nay estimations.
func (b *loopInBuilder) maySwap(_ context.Context, _ Parameters) error {
	return nil
}

// inUse examines our current swap traffic to determine whether we should
// suggest the builder's type of swap for the peer and channels suggested.
func (b *loopInBuilder) inUse(traffic *swapTraffic, peer route.Vertex,
	channels []lnwire.ShortChannelID) error {

	for _, chanID := range channels {
		if traffic.ongoingLoopOut[chanID] {
			log.Debugf("Channel: %v not eligible for suggestions, "+
				"ongoing loop out utilizing channel", chanID)

			return newReasonError(ReasonLoopOut)
		}
	}

	if traffic.ongoingLoopIn[peer] {
		log.Debugf("Peer: %x not eligible for suggestions ongoing "+
			"loop in utilizing peer", peer)

		return newReasonError(ReasonLoopIn)
	}

	lastFail, recentFail := traffic.failedLoopIn[peer]
	if recentFail {
		log.Debugf("Peer: %v not eligible for suggestions, "+
			"was part of a failed swap at: %v", peer,
			lastFail)

		return newReasonError(ReasonFailureBackoff)
	}

	return nil
}

// buildSwap creates a swap for the target peer/channels provided. The autoloop
// boolean indicates whether this swap will actually be executed.
//
// For loop in, we do not add the autoloop label for dry runs.
func (b *loopInBuilder) buildSwap(ctx context.Context, pubkey route.Vertex,
	_ []lnwire.ShortChannelID, amount btcutil.Amount,
	params Parameters) (swapSuggestion, error) {

	quote, err := b.cfg.LoopInQuote(ctx, &loop.LoopInQuoteRequest{
		Amount:         amount,
		LastHop:        &pubkey,
		HtlcConfTarget: params.HtlcConfTarget,
		Initiator:      getInitiator(params),
	})
	if err != nil {
		// If the server fails our quote, we're not reachable right
		// now, so we want to catch this error and fail with a
		// structured error so that we know why we can't swap.
		status, ok := status.FromError(err)
		if ok && status.Code() == codes.FailedPrecondition {
			return nil, newReasonError(ReasonLoopInUnreachable)
		}

		return nil, err
	}

	if err := params.FeeLimit.loopInLimits(amount, quote); err != nil {
		return nil, err
	}

	request := loop.LoopInRequest{
		Amount:         amount,
		MaxSwapFee:     quote.SwapFee,
		MaxMinerFee:    quote.MinerFee,
		HtlcConfTarget: params.HtlcConfTarget,
		LastHop:        &pubkey,
		Initiator:      getInitiator(params),
	}

	if params.Autoloop {
		request.Label = labels.AutoloopLabel(swap.TypeIn)

		if params.EasyAutoloop {
			request.Label = labels.EasyAutoloopLabel(swap.TypeIn)
		}
	}

	return &loopInSwapSuggestion{
		LoopInRequest: request,
	}, nil
}
