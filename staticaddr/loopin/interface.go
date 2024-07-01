package loopin

import (
	"context"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	IdLength = 32
)

// Request contains the required parameters for the swap.
type Request struct {
	// Amount specifies the requested swap amount in sat. This does not
	// include the swap and miner fee.
	Amount btcutil.Amount

	// MaxSwapFee is the maximum we are willing to pay the server for the
	// swap. This value is not disclosed in the swap initiation call, but if
	// the server asks for a higher fee, we abort the swap. Typically this
	// value is taken from the response of the LoopInQuote call. It
	// includes the prepay amount.
	MaxSwapFee btcutil.Amount

	// MaxMinerFee is the maximum in on-chain fees that we are willing to
	// spent. If we publish the on-chain htlc and the fee estimate turns out
	// higher than this value, we cancel the swap.
	//
	// MaxMinerFee is typically taken from the response of the LoopInQuote
	// call.
	MaxMinerFee btcutil.Amount

	// HtlcConfTarget specifies the targeted confirmation target for the
	// client htlc tx.
	HtlcConfTarget int32

	// LastHop optionally specifies the last hop to use for the loop in
	// payment.
	LastHop *route.Vertex

	// ExternalHtlc specifies whether the htlc is published by an external
	// source.
	ExternalHtlc bool

	// Label contains an optional label for the swap.
	Label string

	// Initiator is an optional string that identifies what software
	// initiated the swap (loop CLI, autolooper, LiT UI and so on) and is
	// appended to the user agent string.
	Initiator string

	// Private indicates whether the destination node should be considered
	// private. In which case, loop will generate hophints to assist with
	// probing and payment.
	Private bool

	// RouteHints are optional route hints to reach the destination through
	// private channels.
	RouteHints [][]zpay32.HopHint
}

// AddressManager handles fetching of address parameters.
type AddressManager interface {
	// GetStaticAddressParameters returns the static address parameters.
	GetStaticAddressParameters(ctx context.Context) (*address.Parameters,
		error)

	// GetStaticAddress returns the deposit address for the given
	// client and server public keys.
	GetStaticAddress(ctx context.Context) (*script.StaticAddress, error)

	// ListUnspent returns a list of utxos at the static address.
	ListUnspent(ctx context.Context, minConfs,
		maxConfs int32) ([]*lnwallet.Utxo, error)
}

type DepositManager interface {
	GetActiveDepositsInState(stateFilter fsm.StateType) ([]*deposit.Deposit,
		error)

	AllOutpointsActiveDeposits(outpoints []wire.OutPoint,
		stateFilter fsm.StateType) ([]*deposit.Deposit, bool)

	AllStringOutpointsActiveDeposits(outpoints []string,
		stateFilter fsm.StateType) ([]*deposit.Deposit, bool)

	TransitionDeposits(deposits []*deposit.Deposit, event fsm.EventType,
		expectedFinalState fsm.StateType) error

	UpdateDeposit(d *deposit.Deposit) error
}

type WithdrawalManager interface {
}
