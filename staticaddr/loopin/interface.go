package loopin

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

type (
	// ValidateLoopInContract validates the contract parameters against our
	// request.
	ValidateLoopInContract func(height int32, htlcExpiry int32) error
)

// AddressManager handles fetching of address parameters.
type AddressManager interface {
	// GetStaticAddressParameters returns the static address parameters.
	GetStaticAddressParameters(ctx context.Context) (*address.Parameters,
		error)

	// GetStaticAddress returns the deposit address for the given client and
	// server public keys.
	GetStaticAddress(ctx context.Context) (*script.StaticAddress, error)
}

// DepositManager handles the interaction of loop-ins with deposits.
type DepositManager interface {
	// GetAllDeposits returns all known deposits from the database store.
	GetAllDeposits(ctx context.Context) ([]*deposit.Deposit, error)

	// AllStringOutpointsActiveDeposits returns all deposits that have the
	// given outpoints and are in the given state. If any of the outpoints
	// does not correspond to an active deposit, the function returns false.
	AllStringOutpointsActiveDeposits(outpoints []string,
		stateFilter fsm.StateType) ([]*deposit.Deposit, bool)

	// TransitionDeposits transitions the given deposits to the next state
	// based on the given event. It returns an error if the transition is
	// invalid.
	TransitionDeposits(ctx context.Context, deposits []*deposit.Deposit,
		event fsm.EventType, expectedFinalState fsm.StateType) error
}

// StaticAddressLoopInStore provides access to the static address loop-in DB.
type StaticAddressLoopInStore interface {
	// CreateLoopIn creates a loop-in record in the database.
	CreateLoopIn(ctx context.Context, loopIn *StaticAddressLoopIn) error

	// UpdateLoopIn updates a loop-in record in the database.
	UpdateLoopIn(ctx context.Context, loopIn *StaticAddressLoopIn) error

	// GetStaticAddressLoopInSwapsByStates returns all loop-ins with given
	// states.
	GetStaticAddressLoopInSwapsByStates(ctx context.Context,
		states []fsm.StateType) ([]*StaticAddressLoopIn, error)

	// IsStored checks if the loop-in is already stored in the database.
	IsStored(ctx context.Context, swapHash lntypes.Hash) (bool, error)

	// GetLoopInByHash returns the loop-in swap with the given hash.
	GetLoopInByHash(ctx context.Context, swapHash lntypes.Hash) (
		*StaticAddressLoopIn, error)
}

type QuoteGetter interface {
	// GetLoopInQuote returns a quote for a loop-in swap.
	GetLoopInQuote(ctx context.Context, amt btcutil.Amount,
		pubKey route.Vertex, lastHop *route.Vertex,
		routeHints [][]zpay32.HopHint,
		initiator string, numDeposits uint32) (*loop.LoopInQuote, error)
}
