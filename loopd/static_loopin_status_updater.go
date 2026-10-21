package loopd

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/lightningnetwork/lnd/lntypes"
)

// swapDepositStore loads the deposits that are mapped to a static loop-in.
type swapDepositStore interface {
	// DepositsForSwap returns the deposits that are mapped to the swap
	// with the given hash.
	DepositsForSwap(ctx context.Context,
		swapHash lntypes.Hash) ([]*deposit.Deposit, error)
}

// The loop-in store loads the deposits of a swap for live updates.
var _ swapDepositStore = (*loopin.SqlStore)(nil)

// staticLoopInStatusUpdater publishes static-address loop-in status updates to
// the client-facing swap stream.
type staticLoopInStatusUpdater struct {
	// statusChan sends updates to the client-facing swap stream.
	statusChan chan<- loop.SwapInfo

	// mainCtx is canceled when loopd is shutting down.
	mainCtx context.Context

	// chainParams are used to reconstruct the static loop-in HTLC address.
	chainParams *chaincfg.Params

	// depositStore loads the deposits that are mapped to a swap.
	depositStore swapDepositStore
}

// newStaticLoopInStatusUpdater returns a status updater that sends updates to
// the given channel and reports the on-chain cost of the deposits that the
// deposit store maps to a swap.
func newStaticLoopInStatusUpdater(statusChan chan<- loop.SwapInfo,
	mainCtx context.Context, chainParams *chaincfg.Params,
	depositStore swapDepositStore) *staticLoopInStatusUpdater {

	return &staticLoopInStatusUpdater{
		statusChan:   statusChan,
		mainCtx:      mainCtx,
		chainParams:  chainParams,
		depositStore: depositStore,
	}
}

// sendUpdate converts the persisted static-address loop-in into swap info and
// forwards it to the status stream unless either context is canceled.
func (u *staticLoopInStatusUpdater) sendUpdate(ctx context.Context,
	swp *loopin.StaticAddressLoopIn) error {
	// Native quantities are exposed through the asset variant of the static
	// swap list; the legacy status stream has only Bitcoin amount fields.
	if swp.Asset != nil {
		return nil
	}

	// The on-chain cost is the cost of the deposits mapped to the swap,
	// as in ListStaticAddressSwaps. The swap's own deposits can include
	// deposits that it gave back and another swap took.
	deposits, err := u.depositStore.DepositsForSwap(ctx, swp.SwapHash)
	if err != nil {
		warnf("Unable to load the deposits of static loop-in %v, "+
			"reporting its on-chain cost as unknown: %v",
			swp.SwapHash, err)

		deposits = nil
	}

	info, err := staticAddressLoopInSwapInfoWithChainParams(
		swp, deposits, u.chainParams,
	)
	if err != nil {
		return fmt.Errorf("unable to notify static loop-in update: %w", err)
	}

	select {
	case u.statusChan <- *info:
		return nil

	case <-ctx.Done():
		return ctx.Err()

	case <-u.mainCtx.Done():
		return u.mainCtx.Err()
	}
}
