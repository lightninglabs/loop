package loopdb

import (
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
)

// SwapContract contains the base data that is serialized to persistent storage
// for pending swaps.
type SwapContract struct {
	Preimage        lntypes.Preimage
	AmountRequested btcutil.Amount

	PrepayInvoice string

	SenderKey   [33]byte
	ReceiverKey [33]byte

	CltvExpiry int32

	// MaxPrepayRoutingFee is the maximum off-chain fee in msat that may be
	// paid for the prepayment to the server.
	MaxPrepayRoutingFee btcutil.Amount

	// MaxSwapFee is the maximum we are willing to pay the server for the
	// swap.
	MaxSwapFee btcutil.Amount

	// MaxMinerFee is the maximum in on-chain fees that we are willing to
	// spend.
	MaxMinerFee btcutil.Amount

	// InitiationHeight is the block height at which the swap was
	// initiated.
	InitiationHeight int32

	// InitiationTime is the time at which the swap was initiated.
	InitiationTime time.Time
}
