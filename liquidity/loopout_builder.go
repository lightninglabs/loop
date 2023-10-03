package liquidity

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Compile-time assertion that loopOutBuilder satisfies the swapBuilder
// interface.
var _ swapBuilder = (*loopOutBuilder)(nil)

func newLoopOutBuilder(cfg *Config) *loopOutBuilder {
	return &loopOutBuilder{
		cfg: cfg,
	}
}

type loopOutBuilder struct {
	// cfg contains all the external functionality we require to create
	// swaps.
	cfg *Config
}

// swapType returns the swap type that the builder is responsible for creating.
func (b *loopOutBuilder) swapType() swap.Type {
	return swap.TypeOut
}

// maySwap checks whether we can currently execute a swap, examining the
// current on-chain fee conditions relevant to our swap type against our fee
// restrictions.
//
// For loop out, we check whether the fees required for our on-chain sweep
// transaction exceed our fee limits.
func (b *loopOutBuilder) maySwap(ctx context.Context, params Parameters) error {
	estimate, err := b.cfg.Lnd.WalletKit.EstimateFeeRate(
		ctx, params.SweepConfTarget,
	)
	if err != nil {
		return err
	}

	return params.FeeLimit.mayLoopOut(estimate)
}

// inUse examines our current swap traffic to determine whether we should
// we can perform a swap for the peer/ channels provided.
func (b *loopOutBuilder) inUse(traffic *swapTraffic, peer route.Vertex,
	channels []lnwire.ShortChannelID) error {

	for _, chanID := range channels {
		lastFail, recentFail := traffic.failedLoopOut[chanID]
		if recentFail {
			log.Debugf("Channel: %v not eligible for suggestions, "+
				"was part of a failed swap at: %v", chanID,
				lastFail)

			return newReasonError(ReasonFailureBackoff)
		}

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

	return nil
}

// buildSwap creates a swap for the target peer/channels provided. The autoloop
// boolean indicates whether this swap will actually be executed, because there
// are some calls we can leave out if this swap is just for a dry run (ie, when
// we are just demonstrating the actions that autoloop _would_ take, but not
// actually executing the swap).
//
// For loop out, we don't bother generating a new wallet address if this is a
// dry-run, and we do not add the autoloop label to the recommended swap.
func (b *loopOutBuilder) buildSwap(ctx context.Context, pubkey route.Vertex,
	channels []lnwire.ShortChannelID, amount btcutil.Amount,
	params Parameters) (swapSuggestion, error) {

	quote, err := b.cfg.LoopOutQuote(
		ctx, &loop.LoopOutQuoteRequest{
			Amount:                  amount,
			SweepConfTarget:         params.SweepConfTarget,
			SwapPublicationDeadline: b.cfg.Clock.Now(),
			Initiator:               getInitiator(params),
		},
	)
	if err != nil {
		return nil, err
	}

	log.Debugf("quote for suggestion: %v, swap fee: %v, "+
		"miner fee: %v, prepay: %v", amount, quote.SwapFee,
		quote.MinerFee, quote.PrepayAmount)

	// Check that the estimated fees for the suggested swap are below the
	// fee limits configured.
	if err := params.FeeLimit.loopOutLimits(amount, quote); err != nil {
		return nil, err
	}

	// Break down our fees into appropriate categories for our swap. Our
	// quote does not provide any off-chain routing estimates for us, so
	// we just set our fees from the amounts that we expect to route. We
	// don't have any off-chain fee estimation, so we just use the exact
	// prepay, swap and miner fee provided by the server and split our
	// remaining fees up from there.
	prepayMaxFee, routeMaxFee, minerFee := params.FeeLimit.loopOutFees(
		amount, quote,
	)

	var chanSet loopdb.ChannelSet
	for _, channel := range channels {
		chanSet = append(chanSet, channel.ToUint64())
	}

	// Create a request with our calculated routing fees. We can use the
	// swap fee, prepay amount and miner fee from the quote because we have
	// already validated them.
	request := loop.OutRequest{
		Amount:              amount,
		OutgoingChanSet:     chanSet,
		MaxPrepayRoutingFee: prepayMaxFee,
		MaxSwapRoutingFee:   routeMaxFee,
		MaxMinerFee:         minerFee,
		MaxSwapFee:          quote.SwapFee,
		MaxPrepayAmount:     quote.PrepayAmount,
		SweepConfTarget:     params.SweepConfTarget,
		Initiator:           getInitiator(params),
	}

	if params.Autoloop {
		request.Label = labels.AutoloopLabel(swap.TypeOut)

		if params.EasyAutoloop {
			request.Label = labels.EasyAutoloopLabel(swap.TypeOut)
		}

		account := ""
		addrType := walletrpc.AddressType_WITNESS_PUBKEY_HASH
		if len(params.Account) > 0 {
			account = params.Account
			addrType = params.AccountAddrType
		}
		if params.DestAddr != nil {
			request.DestAddr = params.DestAddr
		} else {
			addr, err := b.cfg.Lnd.WalletKit.NextAddr(
				ctx, account, addrType, false,
			)
			if err != nil {
				return nil, err
			}
			request.DestAddr = addr
		}
	}

	return &loopOutSwapSuggestion{
		OutRequest: request,
	}, nil
}
