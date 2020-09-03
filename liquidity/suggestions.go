package liquidity

import (
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
)

// LoopOutRecommendation contains the information required to recommend a loop
// out.
type LoopOutRecommendation struct {
	// Amount is the total amount to swap.
	Amount btcutil.Amount

	// Channel is the target outgoing channel.
	Channel lnwire.ShortChannelID
}

// newLoopOutRecommendation creates a new loop out swap.
func newLoopOutRecommendation(amount btcutil.Amount,
	channelID lnwire.ShortChannelID) *LoopOutRecommendation {

	return &LoopOutRecommendation{
		Amount:  amount,
		Channel: channelID,
	}
}
