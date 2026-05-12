package loopd

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swapserverrpc"
)

const (
	// htlcConfirmedRecoverySatPerVByte is the fixed fee rate used by the
	// HTLC recovery worker.
	htlcConfirmedRecoverySatPerVByte = 10
)

// htlcConfirmedSubscriber exposes the HTLC-confirmed notification stream used
// by the recovery worker.
type htlcConfirmedSubscriber interface {
	SubscribeHtlcConfirmed(ctx context.Context,
	) <-chan *swapserverrpc.ServerHtlcConfirmedNotification
}

// htlcConfirmedRecoveryManager consumes HTLC-confirmed notifications and
// reuses sweepHtlc to recover the notified loop-out HTLC.
type htlcConfirmedRecoveryManager struct {
	notificationSource htlcConfirmedSubscriber
	swapStore          loopOutStore
	chainParams        *chaincfg.Params
	notifier           htlcChainNotifier
	wallet             htlcWallet
	signer             htlcSigner
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
// attempt for the notified HTLC.
func (m *htlcConfirmedRecoveryManager) handleNotification(ctx context.Context,
	ntfn *swapserverrpc.ServerHtlcConfirmedNotification) {

	if ntfn == nil {
		debugf("Ignoring nil HTLC recovery notification")

		return
	}

	// Require all dependencies up front so the optional recovery path
	// exits quietly when the daemon is not wired for sweeping.
	if m.swapStore == nil || m.chainParams == nil || m.notifier == nil ||
		m.wallet == nil || m.signer == nil {

		debugf("HTLC recovery dependencies unavailable")

		return
	}

	outpoint, htlcAddress, err := m.parseNotification(ntfn)
	if err != nil {
		debugf("Ignoring HTLC recovery notification: %v", err)

		return
	}

	// Reuse sweepHtlc so the worker follows the same success-path spend
	// construction and destination selection as the manual sweep path.
	_, err = sweepHtlc(
		ctx, &looprpc.SweepHtlcRequest{
			Outpoint:    outpoint.String(),
			HtlcAddress: htlcAddress,
			SatPerVbyte: htlcConfirmedRecoverySatPerVByte,
			Publish:     true,
		}, m.chainParams, m.swapStore, m.notifier, m.wallet, m.signer,
	)
	if err != nil {
		debugf("Unable to recover HTLC outpoint %s: %v", outpoint, err)
	}
}

// parseNotification parses the swap hash and outpoint from a notification.
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
