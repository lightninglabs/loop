package liquidity

import (
	"testing"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// TestAutoLoopDisabled tests the case where we need to perform a swap, but
// autoloop is not enabled.
func TestAutoLoopDisabled(t *testing.T) {
	defer test.Guard(t)()

	// Set parameters for a channel that will require a swap.
	channels := []lndclient.ChannelInfo{
		channel1,
	}

	params := defaultParameters
	params.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
		chanID1: chanRule,
	}

	c := newAutoloopTestCtx(t, params, channels, testRestrictions)
	c.start()

	// We expect a single quote to be required for our swap on channel 1.
	// We set its quote to have acceptable fees for our current limit.
	quotes := []quoteRequestResp{
		{
			request: &loop.LoopOutQuoteRequest{
				Amount:          chan1Rec.Amount,
				SweepConfTarget: chan1Rec.SweepConfTarget,
			},
			quote: testQuote,
		},
	}

	// Trigger an autoloop attempt for our test context with no existing
	// loop in/out swaps. We expect a swap for our channel to be suggested,
	// but do not expect any swaps to be executed, since autoloop is
	// disabled by default.
	c.autoloop(1, chan1Rec.Amount+1, nil, quotes, nil)

	// Trigger another autoloop, this time setting our server restrictions
	// to have a minimum swap amount greater than the amount that we need
	// to swap. In this case we don't even expect to get a quote, because
	// our suggested swap is beneath the minimum swap size.
	c.autoloop(chan1Rec.Amount+1, chan1Rec.Amount+2, nil, nil, nil)

	c.stop()
}

// TestAutoLoopEnabled tests enabling the liquidity manger's autolooper. To keep
// the test simple, we do not update actual lnd channel balances, but rather
// run our mock with two channels that will always require a loop out according
// to our rules. This allows us to test the other restrictions placed on the
// autolooper (such as balance, and in-flight swaps) rather than need to worry
// about calculating swap amounts and thresholds.
func TestAutoLoopEnabled(t *testing.T) {
	defer test.Guard(t)()

	var (
		channels = []lndclient.ChannelInfo{
			channel1, channel2,
		}

		swapFeePPM   uint64 = 1000
		routeFeePPM  uint64 = 1000
		prepayFeePPM uint64 = 1000
		prepayAmount        = btcutil.Amount(20000)
		maxMiner            = btcutil.Amount(20000)

		// Create a set of parameters with autoloop enabled. The
		// autoloop budget is set to allow exactly 2 swaps at the prices
		// that we set in our test quotes.
		params = Parameters{
			Autoloop:         true,
			AutoFeeBudget:    40066,
			AutoFeeStartDate: testTime,
			MaxAutoInFlight:  2,
			FailureBackOff:   time.Hour,
			SweepConfTarget:  10,
			FeeLimit: NewFeeCategoryLimit(
				swapFeePPM, routeFeePPM, prepayFeePPM, maxMiner,
				prepayAmount, 20000,
			),
			ChannelRules: map[lnwire.ShortChannelID]*SwapRule{
				chanID1: chanRule,
				chanID2: chanRule,
			},
		}
	)
	c := newAutoloopTestCtx(t, params, channels, testRestrictions)
	c.start()

	// Calculate our maximum allowed fees and create quotes that fall within
	// our budget.
	var (
		amt = chan1Rec.Amount

		maxSwapFee = ppmToSat(amt, swapFeePPM)

		// Create a quote that is within our limits. We do not set miner
		// fee because this value is not actually set by the server.
		quote1 = &loop.LoopOutQuote{
			SwapFee:      maxSwapFee,
			PrepayAmount: prepayAmount - 10,
			MinerFee:     maxMiner - 10,
		}

		quote2 = &loop.LoopOutQuote{
			SwapFee:      maxSwapFee,
			PrepayAmount: prepayAmount - 20,
			MinerFee:     maxMiner - 10,
		}

		quoteRequest = &loop.LoopOutQuoteRequest{
			Amount:          amt,
			SweepConfTarget: params.SweepConfTarget,
		}

		quotes = []quoteRequestResp{
			{
				request: quoteRequest,
				quote:   quote1,
			},
			{
				request: quoteRequest,
				quote:   quote2,
			},
		}

		maxRouteFee = ppmToSat(amt, routeFeePPM)

		chan1Swap = &loop.OutRequest{
			Amount:            amt,
			MaxSwapRoutingFee: maxRouteFee,
			MaxPrepayRoutingFee: ppmToSat(
				quote1.PrepayAmount, prepayFeePPM,
			),
			MaxSwapFee:      quote1.SwapFee,
			MaxPrepayAmount: quote1.PrepayAmount,
			MaxMinerFee:     maxMiner,
			SweepConfTarget: params.SweepConfTarget,
			OutgoingChanSet: loopdb.ChannelSet{chanID1.ToUint64()},
			Label:           labels.AutoloopLabel(swap.TypeOut),
			Initiator:       autoloopSwapInitiator,
		}

		chan2Swap = &loop.OutRequest{
			Amount:            amt,
			MaxSwapRoutingFee: maxRouteFee,
			MaxPrepayRoutingFee: ppmToSat(
				quote2.PrepayAmount, routeFeePPM,
			),
			MaxSwapFee:      quote2.SwapFee,
			MaxPrepayAmount: quote2.PrepayAmount,
			MaxMinerFee:     maxMiner,
			SweepConfTarget: params.SweepConfTarget,
			OutgoingChanSet: loopdb.ChannelSet{chanID2.ToUint64()},
			Label:           labels.AutoloopLabel(swap.TypeOut),
			Initiator:       autoloopSwapInitiator,
		}

		loopOuts = []loopOutRequestResp{
			{
				request: chan1Swap,
				response: &loop.LoopOutSwapInfo{
					SwapHash: lntypes.Hash{1},
				},
			},
			{
				request: chan2Swap,
				response: &loop.LoopOutSwapInfo{
					SwapHash: lntypes.Hash{2},
				},
			},
		}
	)

	// Tick our autolooper with no existing swaps, we expect a loop out
	// swap to be dispatched for each channel.
	c.autoloop(1, amt+1, nil, quotes, loopOuts)

	// Tick again with both of our swaps in progress. We haven't shifted our
	// channel balances at all, so swaps should still be suggested, but we
	// have 2 swaps in flight so we do not expect any suggestion.
	existing := []*loopdb.LoopOut{
		existingSwapFromRequest(chan1Swap, testTime, nil),
		existingSwapFromRequest(chan2Swap, testTime, nil),
	}

	c.autoloop(1, amt+1, existing, nil, nil)

	// Now, we update our channel 2 swap to have failed due to off chain
	// failure and our first swap to have succeeded.
	now := c.testClock.Now()
	failedOffChain := []*loopdb.LoopEvent{
		{
			SwapStateData: loopdb.SwapStateData{
				State: loopdb.StateFailOffchainPayments,
			},
			Time: now,
		},
	}

	success := []*loopdb.LoopEvent{
		{
			SwapStateData: loopdb.SwapStateData{
				State: loopdb.StateSuccess,
				Cost: loopdb.SwapCost{
					Server:  quote1.SwapFee,
					Onchain: maxMiner,
					Offchain: maxRouteFee +
						chan1Rec.MaxPrepayRoutingFee,
				},
			},
			Time: now,
		},
	}

	quotes = []quoteRequestResp{
		{
			request: quoteRequest,
			quote:   quote1,
		},
	}

	loopOuts = []loopOutRequestResp{
		{
			request: chan1Swap,
			response: &loop.LoopOutSwapInfo{
				SwapHash: lntypes.Hash{3},
			},
		},
	}

	existing = []*loopdb.LoopOut{
		existingSwapFromRequest(chan1Swap, testTime, success),
		existingSwapFromRequest(chan2Swap, testTime, failedOffChain),
	}

	// We tick again, this time we expect another swap on channel 1 (which
	// still has balances which reflect that we need to swap), but nothing
	// for channel 2, since it has had a failure.
	c.autoloop(1, amt+1, existing, quotes, loopOuts)

	// Now, we progress our time so that we have sufficiently backed off
	// for channel 2, and could perform another swap.
	c.testClock.SetTime(now.Add(params.FailureBackOff))

	// Our existing swaps (1 successful, one pending) have used our budget
	// so we no longer expect any swaps to automatically dispatch.
	existing = []*loopdb.LoopOut{
		existingSwapFromRequest(chan1Swap, testTime, success),
		existingSwapFromRequest(chan1Swap, c.testClock.Now(), nil),
		existingSwapFromRequest(chan2Swap, testTime, failedOffChain),
	}

	c.autoloop(1, amt+1, existing, quotes, nil)

	c.stop()
}

// TestCompositeRules tests the case where we have rules set on a per peer
// and per channel basis, and perform swaps for both targets.
func TestCompositeRules(t *testing.T) {
	defer test.Guard(t)()

	var (
		// Setup our channels so that we have two channels with peer 2,
		// and a single channel with peer 1.
		channel3 = lndclient.ChannelInfo{
			ChannelID:     chanID3.ToUint64(),
			PubKeyBytes:   peer2,
			LocalBalance:  10000,
			RemoteBalance: 0,
			Capacity:      10000,
		}

		channels = []lndclient.ChannelInfo{
			channel1, channel2, channel3,
		}

		swapFeePPM   uint64 = 1000
		routeFeePPM  uint64 = 1000
		prepayFeePPM uint64 = 1000
		prepayAmount        = btcutil.Amount(20000)
		maxMiner            = btcutil.Amount(20000)

		// Create a set of parameters with autoloop enabled, set our
		// budget to a value that will easily accommodate our two swaps.
		params = Parameters{
			FeeLimit: NewFeeCategoryLimit(
				swapFeePPM, routeFeePPM, prepayFeePPM, maxMiner,
				prepayAmount, 20000,
			),
			Autoloop:         true,
			AutoFeeBudget:    100000,
			AutoFeeStartDate: testTime,
			MaxAutoInFlight:  2,
			FailureBackOff:   time.Hour,
			SweepConfTarget:  10,
			ChannelRules: map[lnwire.ShortChannelID]*SwapRule{
				chanID1: chanRule,
			},
			PeerRules: map[route.Vertex]*SwapRule{
				peer2: chanRule,
			},
		}
	)

	c := newAutoloopTestCtx(t, params, channels, testRestrictions)
	c.start()

	// Calculate our maximum allowed fees and create quotes that fall within
	// our budget.
	var (
		// Create a quote for our peer level swap that is within
		// our budget, with an amount which would balance the peer
		/// across all of its channels.
		peerAmount     = btcutil.Amount(15000)
		maxPeerSwapFee = ppmToSat(peerAmount, swapFeePPM)

		peerSwapQuote = &loop.LoopOutQuote{
			SwapFee:      maxPeerSwapFee,
			PrepayAmount: prepayAmount - 20,
			MinerFee:     maxMiner - 10,
		}

		peerSwapQuoteRequest = &loop.LoopOutQuoteRequest{
			Amount:          peerAmount,
			SweepConfTarget: params.SweepConfTarget,
		}

		maxPeerRouteFee = ppmToSat(peerAmount, routeFeePPM)

		peerSwap = &loop.OutRequest{
			Amount:            peerAmount,
			MaxSwapRoutingFee: maxPeerRouteFee,
			MaxPrepayRoutingFee: ppmToSat(
				peerSwapQuote.PrepayAmount, routeFeePPM,
			),
			MaxSwapFee:      peerSwapQuote.SwapFee,
			MaxPrepayAmount: peerSwapQuote.PrepayAmount,
			MaxMinerFee:     maxMiner,
			SweepConfTarget: params.SweepConfTarget,
			OutgoingChanSet: loopdb.ChannelSet{
				chanID2.ToUint64(), chanID3.ToUint64(),
			},
			Label:     labels.AutoloopLabel(swap.TypeOut),
			Initiator: autoloopSwapInitiator,
		}
		// Create a quote for our single channel swap that is within
		// our budget.
		chanAmount     = chan1Rec.Amount
		maxChanSwapFee = ppmToSat(chanAmount, swapFeePPM)

		channelSwapQuote = &loop.LoopOutQuote{
			SwapFee:      maxChanSwapFee,
			PrepayAmount: prepayAmount - 10,
			MinerFee:     maxMiner - 10,
		}

		chanSwapQuoteRequest = &loop.LoopOutQuoteRequest{
			Amount:          chanAmount,
			SweepConfTarget: params.SweepConfTarget,
		}

		maxChanRouteFee = ppmToSat(chanAmount, routeFeePPM)

		chanSwap = &loop.OutRequest{
			Amount:            chanAmount,
			MaxSwapRoutingFee: maxChanRouteFee,
			MaxPrepayRoutingFee: ppmToSat(
				channelSwapQuote.PrepayAmount, routeFeePPM,
			),
			MaxSwapFee:      channelSwapQuote.SwapFee,
			MaxPrepayAmount: channelSwapQuote.PrepayAmount,
			MaxMinerFee:     maxMiner,
			SweepConfTarget: params.SweepConfTarget,
			OutgoingChanSet: loopdb.ChannelSet{chanID1.ToUint64()},
			Label:           labels.AutoloopLabel(swap.TypeOut),
			Initiator:       autoloopSwapInitiator,
		}
		quotes = []quoteRequestResp{
			{
				request: peerSwapQuoteRequest,
				quote:   peerSwapQuote,
			},
			{
				request: chanSwapQuoteRequest,
				quote:   channelSwapQuote,
			},
		}

		loopOuts = []loopOutRequestResp{
			{
				request: peerSwap,
				response: &loop.LoopOutSwapInfo{
					SwapHash: lntypes.Hash{2},
				},
			},
			{
				request: chanSwap,
				response: &loop.LoopOutSwapInfo{
					SwapHash: lntypes.Hash{1},
				},
			},
		}
	)

	// Tick our autolooper with no existing swaps, we expect a loop out
	// swap to be dispatched for each of our rules. We set our server side
	// maximum to be greater than the swap amount for our peer swap (which
	// is the larger of the two swaps).
	c.autoloop(1, peerAmount+1, nil, quotes, loopOuts)

	c.stop()
}

// existingSwapFromRequest is a helper function which returns the db
// representation of a loop out request with the event set provided.
func existingSwapFromRequest(request *loop.OutRequest, initTime time.Time,
	events []*loopdb.LoopEvent) *loopdb.LoopOut {

	return &loopdb.LoopOut{
		Loop: loopdb.Loop{
			Events: events,
		},
		Contract: &loopdb.LoopOutContract{
			SwapContract: loopdb.SwapContract{
				AmountRequested: request.Amount,
				MaxSwapFee:      request.MaxSwapFee,
				MaxMinerFee:     request.MaxMinerFee,
				InitiationTime:  initTime,
				Label:           request.Label,
			},
			SwapInvoice:         "",
			MaxSwapRoutingFee:   request.MaxSwapRoutingFee,
			SweepConfTarget:     request.SweepConfTarget,
			OutgoingChanSet:     request.OutgoingChanSet,
			MaxPrepayRoutingFee: request.MaxSwapRoutingFee,
		},
	}
}
