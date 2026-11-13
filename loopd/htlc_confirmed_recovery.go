package loopd

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/lntypes"
)

// htlcConfirmedSubscriber exposes the HTLC-confirmed notification stream used
// by the recovery worker.
type htlcConfirmedSubscriber interface {
	SubscribeHtlcConfirmed(ctx context.Context,
	) <-chan *swapserverrpc.ServerHtlcConfirmedNotification
}

// htlcConfirmedRecoveryManager consumes HTLC-confirmed notifications and
// reuses sweepHtlc to recover the notified loop-out HTLC, unless the swap is
// still running in this daemon.
type htlcConfirmedRecoveryManager struct {
	notificationSource htlcConfirmedSubscriber
	swapStore          loopOutStore
	chainParams        *chaincfg.Params
	notifier           htlcChainNotifier
	wallet             htlcWallet
	signer             htlcSigner

	// loopOutRunning reports whether a loop out swap is executing in this
	// daemon. The worker leaves the HTLC of a running swap to that swap,
	// which sweeps it and records the preimage reveal, or has decided not
	// to reveal the preimage. The worker only sweeps directly for swaps
	// that are not running. It is required, without it the worker never
	// sweeps.
	loopOutRunning func(ctx context.Context, hash lntypes.Hash) (bool,
		error)
}

// run starts the HTLC recovery worker.
func (m *htlcConfirmedRecoveryManager) run(ctx context.Context) error {
	if m.notificationSource == nil {
		return nil
	}

	ntfnChan := m.notificationSource.SubscribeHtlcConfirmed(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ntfn, ok := <-ntfnChan:
			if !ok {
				return nil
			}

			m.handleNotification(ctx, ntfn)
		}
	}
}

// handleNotification validates a notification and triggers a direct sweep
// attempt for the notified HTLC. It skips the sweep when the swap is running
// in this daemon or when that cannot be determined.
func (m *htlcConfirmedRecoveryManager) handleNotification(ctx context.Context,
	ntfn *swapserverrpc.ServerHtlcConfirmedNotification) {

	if ntfn == nil {
		debugf("Ignoring nil HTLC recovery notification")

		return
	}

	// Require all dependencies up front so the optional recovery path
	// exits quietly when the daemon is not wired for sweeping.
	if m.swapStore == nil || m.chainParams == nil || m.notifier == nil ||
		m.wallet == nil || m.signer == nil || m.loopOutRunning == nil {

		debugf("HTLC recovery dependencies unavailable")

		return
	}

	outpoint, htlcAddress, err := m.parseNotification(ntfn)
	if err != nil {
		debugf("Ignoring HTLC recovery notification: %v", err)

		return
	}

	// A direct sweep reveals the preimage without the swap recording it.
	// Never do that behind the back of a running loop out. Leave the HTLC
	// to the running swap, which sweeps it or has decided not to reveal
	// the preimage. Look the swap up by its HTLC, not by the notified
	// hash.
	swapHash, err := m.loopOutHashForHtlc(ctx, htlcAddress)
	if err != nil {
		debugf("Ignoring HTLC recovery notification: %v", err)

		return
	}

	// Do not sweep if it is unknown whether the swap is running.
	running, err := m.loopOutRunning(ctx, swapHash)
	if err != nil {
		debugf("Unable to check if swap %v is running: %v", swapHash,
			err)

		return
	}

	if running {
		infof("Swap %v is running, not sweeping HTLC %v directly",
			swapHash, outpoint)

		return
	}

	// Reuse sweepHtlc so the worker follows the same success-path spend
	// construction and destination selection as the manual sweep path.
	_, err = sweepHtlc(
		ctx, &looprpc.SweepHtlcRequest{
			Outpoint:    outpoint.String(),
			HtlcAddress: htlcAddress,
			SatPerVbyte: ntfn.SatPerVbyte,
			Publish:     true,
		}, m.chainParams, m.swapStore, m.notifier, m.wallet, m.signer,
		nil, nil,
	)
	if err != nil {
		debugf("Unable to recover HTLC outpoint %s: %v", outpoint, err)
	}
}

// loopOutHashForHtlc returns the hash of the stored loop out swap whose HTLC
// pays to the given address.
func (m *htlcConfirmedRecoveryManager) loopOutHashForHtlc(ctx context.Context,
	htlcAddress string) (lntypes.Hash, error) {

	htlcAddr, err := btcutil.DecodeAddress(htlcAddress, m.chainParams)
	if err != nil {
		return lntypes.Hash{}, fmt.Errorf("bad HTLC address: %w", err)
	}

	htlcPkScript, err := txscript.PayToAddrScript(htlcAddr)
	if err != nil {
		return lntypes.Hash{}, err
	}

	swaps, err := m.swapStore.FetchLoopOutSwaps(ctx)
	if err != nil {
		return lntypes.Hash{}, err
	}

	for _, swp := range swaps {
		htlc, err := utils.GetHtlc(
			swp.Hash, &swp.Contract.SwapContract, m.chainParams,
		)
		if err != nil {
			return lntypes.Hash{}, err
		}

		if bytes.Equal(htlc.PkScript, htlcPkScript) {
			return swp.Hash, nil
		}
	}

	return lntypes.Hash{}, fmt.Errorf("no swap found for HTLC address %v",
		htlcAddress)
}

// parseNotification parses the HTLC outpoint and address from a
// notification.
func (m *htlcConfirmedRecoveryManager) parseNotification(
	ntfn *swapserverrpc.ServerHtlcConfirmedNotification) (*wire.OutPoint,
	string, error) {

	outpoint, err := wire.NewOutPointFromString(ntfn.HtlcOutpoint)
	if err != nil {
		return nil, "", fmt.Errorf("bad outpoint: %w", err)
	}

	if ntfn.HtlcAddress == "" {
		return nil, "", fmt.Errorf("missing HTLC address")
	}

	return outpoint, ntfn.HtlcAddress, nil
}
