package loopd

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/staticaddr/loopin"
)

// staticLoopInStatusUpdater publishes static-address loop-in status updates to
// the client-facing swap stream.
type staticLoopInStatusUpdater struct {
	// statusChan sends updates to the client-facing swap stream.
	statusChan chan<- loop.SwapInfo

	// mainCtx is canceled when loopd is shutting down.
	mainCtx context.Context

	// chainParams are used to reconstruct the static loop-in HTLC address.
	chainParams *chaincfg.Params
}

// sendUpdate converts the persisted static-address loop-in into swap info and
// forwards it to the status stream unless either context is canceled.
func (u *staticLoopInStatusUpdater) sendUpdate(ctx context.Context,
	swp *loopin.StaticAddressLoopIn) error {

	info, err := staticAddressLoopInSwapInfoWithChainParams(
		swp, u.chainParams,
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
