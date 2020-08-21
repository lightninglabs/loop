package liquidity

import (
	"sort"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

type SwapRecommendation interface {
	// SwapType returns the type of swap we recommend.
	SwapType() swap.Type

	// Amount returns the total amount recommend swapping.
	Amount() btcutil.Amount
}

// swapRecommendationFunc is the signature of a function which is used to create
// a swap recommendation.
type swapRecommendationFunc func(btcutil.Amount,
	channelDetails) SwapRecommendation

// LoopOutRecommendation contains the information required to recommend a loop
// out.
type LoopOutRecommendation struct {
	// amount is the total amount to swap.
	amount btcutil.Amount

	// Channel is the target outgoing channel.
	Channel lnwire.ShortChannelID
}

// SwapType returns the type of swap we recommend.
func (o *LoopOutRecommendation) SwapType() swap.Type {
	return swap.TypeOut
}

// Amount returns the total amount recommend swapping.
func (o *LoopOutRecommendation) Amount() btcutil.Amount {
	return o.amount
}

// newLoopOutRecommendation creates a new loop out swap. It returns an interface
// rather that a reference to the struct so that it can be used as type
// swapRecommendationFunc.
func newLoopOutRecommendation(amount btcutil.Amount,
	channel channelDetails) SwapRecommendation {

	return &LoopOutRecommendation{
		amount:  amount,
		Channel: channel.channel,
	}
}

// LoopOutRecommendation contains the information required to recommend a loop
// in.
type LoopInRecommendation struct {
	// amount is the total amount to swap.
	amount btcutil.Amount

	// LastHop is the public key of the peer that we want the last hop of
	// the swap to be restricted to.
	LastHop route.Vertex
}

// SwapType returns the type of swap we recommend.
func (i *LoopInRecommendation) SwapType() swap.Type {
	return swap.TypeIn
}

// Amount returns the total amount recommend swapping.
func (i *LoopInRecommendation) Amount() btcutil.Amount {
	return i.amount
}

// newLoopInRecommendation creates a new loop out swap. It returns an interface
// rather that a reference to the struct so that it can be used as type
// swapRecommendationFunc.
func newLoopInRecommendation(amount btcutil.Amount,
	channel channelDetails) SwapRecommendation {

	return &LoopInRecommendation{
		amount:  amount,
		LastHop: channel.peer,
	}
}

// selectSwaps takes a set of channels that collectively have surplus balance
// available, and an amount that can be shifted without overshooting and
// unbalancing the channels in the opposite direction and returns a slice of
// swaps which make up this total amount, taking into account the limitations
// places on swap size. This function assumes that we will be performing our
// swap payment with a single htlc, so does not attempt to split our amount
// across channels. It breaks our swap up into multiple swaps if the amount we
// require is more than the maximum swap size. This function will only recommend
// one swap per channel. This means that if one channel has all the available
// surplus, we will not be able to swap the full amount in one go.
func selectSwaps(channels []channelDetails,
	makeSwap swapRecommendationFunc, amount, minSwapAmount,
	maxSwapAmount btcutil.Amount) []SwapRecommendation {

	// Sort our channels from most to least available surplus.
	sort.SliceStable(channels, func(i, j int) bool {
		return channels[i].amount > channels[j].amount
	})

	var swaps []SwapRecommendation

	for _, channel := range channels {
		availableAmt := channel.amount

		// If the available amount is smaller than the minimum amount we
		// can swap, we cannot use this chanel.
		if availableAmt < minSwapAmount {
			continue
		}

		// If we have more available in this channel than we need, we
		// just aim to swap our total amount.
		if availableAmt > amount {
			availableAmt = amount
		}

		// If the surplus amount is more than our maximum amount, we
		// set our swap amount to the full surplus, otherwise we just
		// use our maximum amount.
		swapAmt := maxSwapAmount
		if availableAmt < maxSwapAmount {
			swapAmt = availableAmt
		}

		// Add a swap with this amount to our set of recommended swaps.
		swaps = append(swaps, makeSwap(swapAmt, channel))

		// Subtract this rounded swap amount from the total we will need
		// to swap.
		amount -= swapAmt

		// Once our swap amount falls under the minimum swap amount, we
		// can break our loop because we cannot swap any further.
		if amount < minSwapAmount {
			break
		}
	}

	return swaps
}
