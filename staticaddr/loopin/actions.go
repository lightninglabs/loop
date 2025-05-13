package loopin

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	defaultConfTarget = 3

	DefaultPaymentTimeoutSeconds = 60
)

var (
	// ErrFeeTooHigh is returned if the server sets a fee rate for the htlc
	// tx that is too high. We prevent here against a low htlc timeout sweep
	// amount.
	ErrFeeTooHigh = errors.New("server htlc tx fee is higher than the " +
		"configured allowed maximum")

	// ErrBackupFeeTooHigh is returned if the server sets a fee rate for the
	// htlc backup tx that is too high. We prevent here against a low htlc
	// timeout sweep amount.
	ErrBackupFeeTooHigh = errors.New("server htlc backup tx fee is " +
		"higher than the configured allowed maximum")
)

// InitHtlcAction is executed if all loop-in information has been validated. We
// assemble a loop-in request and send it to the server.
func (f *FSM) InitHtlcAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	// Lock the deposits and transition them to the LoopingIn state.
	err := f.cfg.DepositManager.TransitionDeposits(
		ctx, f.loopIn.Deposits, deposit.OnLoopInInitiated,
		deposit.LoopingIn,
	)
	if err != nil {
		err = fmt.Errorf("unable to loop-in deposits: %w", err)

		return f.HandleError(err)
	}

	// Calculate the swap invoice amount. The server needs to pay us the
	// sum of all deposits minus the fees that the server charges for the
	// swap.
	swapInvoiceAmt := f.loopIn.TotalDepositAmount() - f.loopIn.QuotedSwapFee

	// Generate random preimage.
	var swapPreimage lntypes.Preimage
	if _, err = rand.Read(swapPreimage[:]); err != nil {
		err = fmt.Errorf("unable to create random swap preimage: %w",
			err)

		return f.HandleError(err)
	}
	f.loopIn.SwapPreimage = swapPreimage
	f.loopIn.SwapHash = swapPreimage.Hash()

	// Derive a client key for the HTLC.
	keyDesc, err := f.cfg.WalletKit.DeriveNextKey(
		ctx, swap.StaticAddressKeyFamily,
	)
	if err != nil {
		err = fmt.Errorf("unable to derive client htlc key: %w", err)

		return f.HandleError(err)
	}
	f.loopIn.ClientPubkey = keyDesc.PubKey
	f.loopIn.HtlcKeyLocator = keyDesc.KeyLocator

	// Create the swap invoice in lnd.
	_, swapInvoice, err := f.cfg.LndClient.AddInvoice(
		ctx, &invoicesrpc.AddInvoiceData{
			Preimage:   &swapPreimage,
			Value:      lnwire.NewMSatFromSatoshis(swapInvoiceAmt),
			Memo:       "static address loop-in",
			Expiry:     3600 * 24 * 365,
			RouteHints: f.loopIn.RouteHints,
			Private:    true,
		},
	)
	if err != nil {
		err = fmt.Errorf("unable to create swap invoice: %w", err)

		return f.HandleError(err)
	}
	f.loopIn.SwapInvoice = swapInvoice

	f.loopIn.ProtocolVersion = version.AddressProtocolVersion(
		version.CurrentRPCProtocolVersion(),
	)

	loopInReq := &swapserverrpc.ServerStaticAddressLoopInRequest{
		SwapHash:              f.loopIn.SwapHash[:],
		DepositOutpoints:      f.loopIn.DepositOutpoints,
		HtlcClientPubKey:      f.loopIn.ClientPubkey.SerializeCompressed(),
		SwapInvoice:           f.loopIn.SwapInvoice,
		ProtocolVersion:       version.CurrentRPCProtocolVersion(),
		UserAgent:             loop.UserAgent(f.loopIn.Initiator),
		PaymentTimeoutSeconds: f.loopIn.PaymentTimeoutSeconds,
	}
	if f.loopIn.LastHop != nil {
		loopInReq.LastHop = f.loopIn.LastHop
	}

	loopInResp, err := f.cfg.Server.ServerStaticAddressLoopIn(
		ctx, loopInReq,
	)
	if err != nil {
		err = fmt.Errorf("unable to initiate the loop-in with the "+
			"server: %w", err)

		return f.HandleError(err)
	}

	// Pushing empty sigs signals the server that we abandoned the swap
	// attempt.
	pushEmptySigs := func() {
		_, err = f.cfg.Server.PushStaticAddressHtlcSigs(
			ctx, &swapserverrpc.PushStaticAddressHtlcSigsRequest{
				SwapHash: f.loopIn.SwapHash[:],
			},
		)
		if err != nil {
			log.Warnf("unable to push htlc tx sigs to server: %v",
				err)
		}
	}

	serverPubkey, err := btcec.ParsePubKey(loopInResp.HtlcServerPubKey)
	if err != nil {
		pushEmptySigs()
		err = fmt.Errorf("unable to parse server pubkey: %w", err)

		return f.HandleError(err)
	}
	f.loopIn.ServerPubkey = serverPubkey

	// Validate if the response parameters are outside our allowed range
	// preventing us from continuing with a swap.
	err = f.cfg.ValidateLoopInContract(
		int32(f.loopIn.InitiationHeight), loopInResp.HtlcExpiry,
	)
	if err != nil {
		pushEmptySigs()
		err = fmt.Errorf("server response parameters are outside "+
			"our allowed range: %w", err)

		return f.HandleError(err)
	}

	f.loopIn.HtlcCltvExpiry = loopInResp.HtlcExpiry
	f.htlcServerNonces, err = toNonces(loopInResp.StandardHtlcInfo.Nonces)
	if err != nil {
		pushEmptySigs()
		err = fmt.Errorf("unable to convert server nonces: %w", err)

		return f.HandleError(err)
	}
	f.htlcServerNoncesHighFee, err = toNonces(
		loopInResp.HighFeeHtlcInfo.Nonces,
	)
	if err != nil {
		pushEmptySigs()

		return f.HandleError(err)
	}
	f.htlcServerNoncesExtremelyHighFee, err = toNonces(
		loopInResp.ExtremeFeeHtlcInfo.Nonces,
	)
	if err != nil {
		pushEmptySigs()

		return f.HandleError(err)
	}

	// We need to defend against the server setting high fees for the htlc
	// tx since we might have to sweep the timeout path. We maximally allow
	// a configured percentage of the swap value to be spent on fees.
	amt := float64(f.loopIn.TotalDepositAmount())
	maxHtlcTxFee := btcutil.Amount(amt *
		f.cfg.MaxStaticAddrHtlcFeePercentage)

	maxHtlcTxBackupFee := btcutil.Amount(amt *
		f.cfg.MaxStaticAddrHtlcBackupFeePercentage)

	feeRate := chainfee.SatPerKWeight(loopInResp.StandardHtlcInfo.FeeRate)
	fee := feeRate.FeeForWeight(f.loopIn.htlcWeight())
	if fee > maxHtlcTxFee {
		// Abort the swap by pushing empty sigs to the server.
		pushEmptySigs()

		log.Errorf("server htlc tx fee is higher than the configured "+
			"allowed maximum: %v > %v", fee, maxHtlcTxFee)

		return f.HandleError(ErrFeeTooHigh)
	}
	f.loopIn.HtlcTxFeeRate = feeRate

	highFeeRate := chainfee.SatPerKWeight(loopInResp.HighFeeHtlcInfo.FeeRate)
	fee = highFeeRate.FeeForWeight(f.loopIn.htlcWeight())
	if fee > maxHtlcTxBackupFee {
		// Abort the swap by pushing empty sigs to the server.
		pushEmptySigs()

		log.Errorf("server htlc backup tx fee is higher than the "+
			"configured allowed maximum: %v > %v", fee,
			maxHtlcTxBackupFee)

		return f.HandleError(ErrFeeTooHigh)
	}
	f.loopIn.HtlcTxHighFeeRate = highFeeRate

	extremelyHighFeeRate := chainfee.SatPerKWeight(
		loopInResp.ExtremeFeeHtlcInfo.FeeRate,
	)
	fee = extremelyHighFeeRate.FeeForWeight(f.loopIn.htlcWeight())
	if fee > maxHtlcTxBackupFee {
		// Abort the swap by pushing empty sigs to the server.
		pushEmptySigs()

		log.Errorf("server htlc backup tx fee is higher than the "+
			"configured allowed maximum: %v > %v", fee,
			maxHtlcTxBackupFee)

		return f.HandleError(ErrFeeTooHigh)
	}
	f.loopIn.HtlcTxExtremelyHighFeeRate = extremelyHighFeeRate

	// Derive the sweep address for the htlc timeout sweep tx.
	sweepAddress, err := f.cfg.WalletKit.NextAddr(
		ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		pushEmptySigs()
		err = fmt.Errorf("unable to derive htlc timeout sweep "+
			"address: %w", err)

		return f.HandleError(err)
	}
	f.loopIn.HtlcTimeoutSweepAddress = sweepAddress

	// Once the htlc tx is initiated, we store the loop-in in the database.
	err = f.cfg.Store.CreateLoopIn(ctx, f.loopIn)
	if err != nil {
		pushEmptySigs()
		err = fmt.Errorf("unable to store loop-in in db: %w", err)

		return f.HandleError(err)
	}

	return OnHtlcInitiated
}

// SignHtlcTxAction is called if the htlc was initialized and the server
// provided the necessary information to construct the htlc tx. We sign the htlc
// tx and send the signatures to the server.
func (f *FSM) SignHtlcTxAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	var err error

	f.loopIn.AddressParams, err =
		f.cfg.AddressManager.GetStaticAddressParameters(ctx)

	if err != nil {
		err = fmt.Errorf("unable to get static address parameters: "+
			"%w", err)

		return f.HandleError(err)
	}

	f.loopIn.Address, err = f.cfg.AddressManager.GetStaticAddress(ctx)
	if err != nil {
		err = fmt.Errorf("unable to get static address: %w", err)

		return f.HandleError(err)
	}

	// Create a musig2 session for each deposit and different htlc tx fee
	// rates.
	createSession := f.loopIn.createMusig2Sessions
	htlcSessions, clientHtlcNonces, err := createSession(ctx, f.cfg.Signer)
	if err != nil {
		err = fmt.Errorf("unable to create musig2 sessions: %w", err)

		return f.HandleError(err)
	}
	defer f.cleanUpSessions(ctx, htlcSessions)

	htlcSessionsHighFee, highFeeNonces, err := createSession(
		ctx, f.cfg.Signer,
	)
	if err != nil {
		return f.HandleError(err)
	}
	defer f.cleanUpSessions(ctx, htlcSessionsHighFee)

	htlcSessionsExtremelyHighFee, extremelyHighNonces, err := createSession(
		ctx, f.cfg.Signer,
	)
	if err != nil {
		err = fmt.Errorf("unable to convert nonces: %w", err)
		return f.HandleError(err)
	}
	defer f.cleanUpSessions(ctx, htlcSessionsExtremelyHighFee)

	// Create the htlc txns for different fee rates.
	htlcTx, err := f.loopIn.createHtlcTx(
		f.cfg.ChainParams, f.loopIn.HtlcTxFeeRate,
		f.cfg.MaxStaticAddrHtlcFeePercentage,
	)
	if err != nil {
		return f.HandleError(err)
	}
	htlcTxHighFee, err := f.loopIn.createHtlcTx(
		f.cfg.ChainParams, f.loopIn.HtlcTxHighFeeRate,
		f.cfg.MaxStaticAddrHtlcBackupFeePercentage,
	)
	if err != nil {
		return f.HandleError(err)
	}
	htlcTxExtremelyHighFee, err := f.loopIn.createHtlcTx(
		f.cfg.ChainParams, f.loopIn.HtlcTxExtremelyHighFeeRate,
		f.cfg.MaxStaticAddrHtlcBackupFeePercentage,
	)
	if err != nil {
		err = fmt.Errorf("unable to create the htlc tx: %w", err)
		return f.HandleError(err)
	}

	// Next we'll get our htlc tx signatures for different fee rates.
	htlcSigs, err := f.loopIn.signMusig2Tx(
		ctx, htlcTx, f.cfg.Signer, htlcSessions, f.htlcServerNonces,
	)
	if err != nil {
		err = fmt.Errorf("unable to sign htlc tx: %w", err)
		return f.HandleError(err)
	}

	htlcSigsHighFee, err := f.loopIn.signMusig2Tx(
		ctx, htlcTxHighFee, f.cfg.Signer, htlcSessionsHighFee,
		f.htlcServerNoncesHighFee,
	)
	if err != nil {
		return f.HandleError(err)
	}
	htlcSigsExtremelyHighFee, err := f.loopIn.signMusig2Tx(
		ctx, htlcTxExtremelyHighFee, f.cfg.Signer,
		htlcSessionsExtremelyHighFee, f.htlcServerNoncesExtremelyHighFee,
	)
	if err != nil {
		return f.HandleError(err)
	}

	// Push htlc tx sigs to server.
	pushHtlcReq := &swapserverrpc.PushStaticAddressHtlcSigsRequest{
		SwapHash: f.loopIn.SwapHash[:],
		StandardHtlcInfo: &swapserverrpc.ClientHtlcSigningInfo{
			Nonces: clientHtlcNonces,
			Sigs:   htlcSigs,
		},
		HighFeeHtlcInfo: &swapserverrpc.ClientHtlcSigningInfo{
			Nonces: highFeeNonces,
			Sigs:   htlcSigsHighFee,
		},
		ExtremeFeeHtlcInfo: &swapserverrpc.ClientHtlcSigningInfo{
			Nonces: extremelyHighNonces,
			Sigs:   htlcSigsExtremelyHighFee,
		},
	}
	_, err = f.cfg.Server.PushStaticAddressHtlcSigs(ctx, pushHtlcReq)
	if err != nil {
		err = fmt.Errorf("unable to push htlc tx sigs to server: %w",
			err)

		return f.HandleError(err)
	}

	// Note:
	// From here on we need to monitor for the htlc tx hitting the chain
	// until the invoice is settled because the server can now publish the
	// htlc tx without paying the invoice. In this case we need to wait till
	// the htlc times out and then sweep it back to us.
	return OnHtlcTxSigned
}

// cleanUpSessions releases allocated memory of the musig2 sessions.
func (f *FSM) cleanUpSessions(ctx context.Context,
	sessions []*input.MuSig2SessionInfo) {

	for _, s := range sessions {
		err := f.cfg.Signer.MuSig2Cleanup(
			context.WithoutCancel(ctx), s.SessionID,
		)
		if err != nil {
			f.Warnf("unable to cleanup musig2 session: %v", err)
		}
	}
}

// MonitorInvoiceAndHtlcTxAction is called after the htlc tx has been signed by
// us. The server from here on has the ability to publish the htlc tx. If the
// server publishes the htlc tx without paying the invoice, we have to monitor
// for the timeout path and sweep the funds back to us. If, while waiting for
// the htlc timeout, our invoice gets paid, the swap is considered successful,
// and we can stop monitoring the htlc confirmation and continue to sign the
// sweepless sweep.
func (f *FSM) MonitorInvoiceAndHtlcTxAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	// Subscribe to the state of the swap invoice. If upon restart recovery,
	// we land here and observe that the invoice is already canceled, it can
	// only be the case where a user-provided payment timeout was hit, the
	// invoice got canceled and the timeout of the htlc was not reached yet.
	// So we want to wait until the htlc timeout path opens up so that we
	// could sweep the funds back to us if the server published it without
	// paying the invoice.
	subscribeCtx, cancelInvoiceSubscription := context.WithCancel(ctx)
	defer cancelInvoiceSubscription()

	invoiceUpdateChan, invoiceErrChan, err :=
		f.cfg.InvoicesClient.SubscribeSingleInvoice(
			subscribeCtx, f.loopIn.SwapHash,
		)
	if err != nil {
		err = fmt.Errorf("unable to subscribe to swap "+
			"invoice: %w", err)

		return f.HandleError(err)
	}

	htlc, err := f.loopIn.getHtlc(f.cfg.ChainParams)
	if err != nil {
		err = fmt.Errorf("unable to get htlc: %w", err)

		return f.HandleError(err)
	}

	// Subscribe to htlc tx confirmation.
	reorgChan := make(chan struct{}, 1)
	registerHtlcConf := func() (chan *chainntnfs.TxConfirmation, chan error,
		error) {

		return f.cfg.ChainNotifier.RegisterConfirmationsNtfn(
			ctx, nil, htlc.PkScript, defaultConfTarget,
			int32(f.loopIn.InitiationHeight),
			lndclient.WithReOrgChan(reorgChan),
		)
	}

	htlcConfChan, htlcErrConfChan, err := registerHtlcConf()
	if err != nil {
		err = fmt.Errorf("unable to monitor htlc tx confirmation: %w",
			err)

		return f.HandleError(err)
	}

	// Subscribe to new blocks.
	registerBlocks := f.cfg.ChainNotifier.RegisterBlockEpochNtfn
	blockChan, blockChanErr, err := registerBlocks(ctx)
	if err != nil {
		err = fmt.Errorf("unable to subscribe to new blocks: %w", err)

		return f.HandleError(err)
	}

	htlcConfirmed := false

	invoice, err := f.cfg.LndClient.LookupInvoice(ctx, f.loopIn.SwapHash)
	if err != nil {
		err = fmt.Errorf("unable to look up invoice by swap hash: %w",
			err)

		return f.HandleError(err)
	}

	// Create the swap payment timeout timer. If it runs out we cancel the
	// invoice, but keep monitoring the htlc confirmation.
	// If the invoice was canceled, e.g. before a restart, we don't need to
	// set a new deadline.
	var deadlineChan <-chan time.Time
	if invoice.State != invoices.ContractCanceled {
		// If the invoice is still live we set the timeout to the
		// remaining payment time. If too much time has elapsed, e.g.
		// after a restart, we set the timeout to 0 to cancel the
		// invoice and unlock the deposits immediately.
		remainingTimeSeconds := f.loopIn.RemainingPaymentTimeSeconds()

		// If the invoice isn't cancelled yet and the payment timeout
		// elapsed, we set the timeout to 0 to cancel the invoice and
		// unlock the deposits immediately. Otherwise, we start the
		// timer with the remaining seconds to timeout.
		timeout := time.Duration(0) * time.Second
		if remainingTimeSeconds > 0 {
			timeout = time.Duration(remainingTimeSeconds) *
				time.Second
		}

		deadlineChan = time.NewTimer(timeout).C
	} else {
		// If the invoice was canceled previously we end our
		// subscription to invoice updates.
		cancelInvoiceSubscription()
	}

	cancelInvoice := func() {
		f.Errorf("timeout waiting for invoice to be " +
			"paid, canceling invoice")

		// Cancel the lndclient invoice subscription.
		cancelInvoiceSubscription()

		err = f.cfg.InvoicesClient.CancelInvoice(ctx, f.loopIn.SwapHash)
		if err != nil {
			f.Warnf("unable to cancel invoice "+
				"for swap hash: %v", err)
		}
	}

	for {
		select {
		case <-htlcConfChan:
			f.Infof("htlc tx confirmed")

			htlcConfirmed = true

		case err = <-htlcErrConfChan:
			f.Errorf("htlc tx conf chan error: %v", err)

		case <-reorgChan:
			// A reorg happened. We invalidate a previous htlc
			// confirmation and re-register for the next
			// confirmation.
			htlcConfirmed = false

			htlcConfChan, htlcErrConfChan, err = registerHtlcConf()
			if err != nil {
				f.Errorf("unable to monitor htlc tx "+
					"confirmation: %v", err)
			}

		case <-deadlineChan:
			// If the server didn't pay the invoice on time, we
			// cancel the invoice and keep monitoring the htlc tx
			// confirmation. We also need to unlock the deposits to
			// re-enable them for loop-ins and withdrawals.
			cancelInvoice()

			event := f.UnlockDepositsAction(ctx, nil)
			if event != fsm.OnError {
				f.Errorf("unable to unlock deposits after " +
					"payment deadline")
			}

		case currentHeight := <-blockChan:
			// If the htlc is confirmed but blockChan fires before
			// htlcConfChan, we would wrongfully assume that the
			// htlc tx was not confirmed which would lead to
			// returning OnSwapTimedOut in the code below. This in
			// turn would prevent us from sweeping the htlc timeout
			// path back to us.
			// Hence, we delay the timeout check here by one block
			// to ensure that htlcConfChan fires first.
			if !f.loopIn.isHtlcTimedOut(currentHeight - 1) {
				// If the htlc hasn't timed out yet, we continue
				// monitoring the htlc confirmation and the
				// invoice settlement.
				continue
			}

			f.Infof("htlc timed out at block height %v",
				currentHeight)

			// If the timeout path opened up we consider the swap
			// failed and cancel the invoice.
			cancelInvoice()

			if !htlcConfirmed {
				f.Infof("swap timed out, htlc not confirmed")

				// If the htlc hasn't confirmed but the timeout
				// path opened up, and we didn't receive the
				// swap payment, we consider the swap attempt to
				// be failed. We cancelled the invoice, but
				// don't need to unlock the deposits because
				// that happened when the payment deadline was
				// reached.
				return OnSwapTimedOut
			}

			// If the htlc has confirmed and the timeout path has
			// opened up we sweep the funds back to us.
			err = f.cfg.DepositManager.TransitionDeposits(
				ctx, f.loopIn.Deposits,
				deposit.OnSweepingHtlcTimeout,
				deposit.SweepHtlcTimeout,
			)
			if err != nil {
				log.Errorf("unable to transition "+
					"deposits to the htlc timeout "+
					"sweeping state: %v", err)
			}

			return OnSweepHtlcTimeout

		case err = <-blockChanErr:
			f.Errorf("block subscription error: %v", err)

			return f.HandleError(err)

		case update := <-invoiceUpdateChan:
			switch update.State {
			case invoices.ContractOpen:
			case invoices.ContractAccepted:
			case invoices.ContractSettled:
				f.Debugf("received off-chain payment update "+
					"%v", update.State)

				return OnPaymentReceived

			case invoices.ContractCanceled:
				// If the invoice was canceled we only log here
				// since we still need to monitor until the htlc
				// timed out.
				log.Warnf("invoice for swap hash %v canceled",
					f.loopIn.SwapHash)

			default:
				err = fmt.Errorf("unexpected invoice state %v "+
					"for swap hash %v canceled",
					update.State, f.loopIn.SwapHash)

				return f.HandleError(err)
			}

		case err = <-invoiceErrChan:
			f.Errorf("invoice subscription error: %v", err)

		case <-ctx.Done():
			return f.HandleError(ctx.Err())
		}
	}
}

// SweepHtlcTimeoutAction is called if the server published the htlc tx without
// paying the invoice. We wait for the timeout path to open up and sweep the
// funds back to us.
func (f *FSM) SweepHtlcTimeoutAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	for {
		err := f.createAndPublishHtlcTimeoutSweepTx(ctx)
		if err == nil {
			break
		}

		f.Errorf("unable to create and publish htlc timeout sweep "+
			"tx: %v, retrying in  %v", err, time.Hour.String())

		select {
		case <-ctx.Done():
			f.Errorf(ctx.Err().Error())

		default:
			<-time.After(1 * time.Hour)
		}
	}

	return OnHtlcTimeoutSweepPublished
}

// MonitorHtlcTimeoutSweepAction is called after the htlc timeout sweep tx has
// been published. We monitor the confirmation of the htlc timeout sweep tx and
// finalize the deposits once swept.
func (f *FSM) MonitorHtlcTimeoutSweepAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	f.Infof("monitoring htlc timeout sweep tx %v",
		f.loopIn.HtlcTimeoutSweepTxHash)

	timeoutSweepPkScript, err := txscript.PayToAddrScript(
		f.loopIn.HtlcTimeoutSweepAddress,
	)
	if err != nil {
		err = fmt.Errorf("unable to convert timeout sweep address to "+
			"pkscript: %w", err)

		return f.HandleError(err)
	}

	htlcTimeoutTxidChan, errChan, err :=
		f.cfg.ChainNotifier.RegisterConfirmationsNtfn(
			ctx, f.loopIn.HtlcTimeoutSweepTxHash,
			timeoutSweepPkScript, defaultConfTarget,
			int32(f.loopIn.InitiationHeight),
		)

	if err != nil {
		err = fmt.Errorf("unable to register to the htlc timeout "+
			"sweep tx: %w", err)

		return f.HandleError(err)
	}

	for {
		select {
		case err := <-errChan:
			return f.HandleError(err)

		case conf := <-htlcTimeoutTxidChan:
			err = f.cfg.DepositManager.TransitionDeposits(
				ctx, f.loopIn.Deposits,
				deposit.OnHtlcTimeoutSwept,
				deposit.HtlcTimeoutSwept,
			)
			if err != nil {
				err = fmt.Errorf("unable to transition the "+
					"deposits to the htlc timeout swept "+
					"state: %w", err)

				return f.HandleError(err)
			}

			f.Infof("htlc timeout sweep tx got %d confirmations "+
				"at block %d", defaultConfTarget,
				conf.BlockHeight-defaultConfTarget+1)

			return OnHtlcTimeoutSwept

		case <-ctx.Done():
			return f.HandleError(ctx.Err())
		}
	}
}

// PaymentReceivedAction is called if the invoice was settled. We finalize the
// deposits by transitioning them to the LoopedIn state.
func (f *FSM) PaymentReceivedAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	// Unlock the deposits and transition them to the LoopedIn state.
	err := f.cfg.DepositManager.TransitionDeposits(
		ctx, f.loopIn.Deposits, deposit.OnLoopedIn, deposit.LoopedIn,
	)
	if err != nil {
		err = fmt.Errorf("payment received, but unable to transition "+
			"deposits into the final state: %w", err)

		return f.HandleError(err)
	}

	return OnSucceeded
}

// UnlockDepositsAction is called if the loop-in failed and its deposits should
// be available in a future loop-in request.
func (f *FSM) UnlockDepositsAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	err := f.cfg.DepositManager.TransitionDeposits(
		ctx, f.loopIn.Deposits, fsm.OnError, deposit.Deposited,
	)
	if err != nil {
		err = fmt.Errorf("unable to unlock deposits: %w", err)

		return f.HandleError(err)
	}

	return fsm.OnError
}

// createAndPublishHtlcTimeoutSweepTx creates and publishes the htlc timeout
// sweep transaction.
func (f *FSM) createAndPublishHtlcTimeoutSweepTx(ctx context.Context) error {
	// Get a fee rate.
	feeRate, err := f.cfg.WalletKit.EstimateFeeRate(ctx, defaultConfTarget)
	if err != nil {
		return err
	}

	getInfo, err := f.cfg.LndClient.GetInfo(ctx)
	if err != nil {
		return err
	}

	// Create htlc timeout transaction.
	timeoutTx, err := f.loopIn.createHtlcSweepTx(
		ctx, f.cfg.Signer, f.loopIn.HtlcTimeoutSweepAddress, feeRate,
		f.cfg.ChainParams, getInfo.BlockHeight,
		f.cfg.MaxStaticAddrHtlcFeePercentage,
	)
	if err != nil {
		return fmt.Errorf("unable to create htlc timeout sweep tx: %w",
			err)
	}

	// Broadcast htlc timeout transaction.
	txLabel := fmt.Sprintf(
		"htlc-timeout-sweep-%v", f.loopIn.SwapHash,
	)

	err = f.cfg.WalletKit.PublishTransaction(ctx, timeoutTx, txLabel)
	if err != nil {
		e := err.Error()
		if !strings.Contains(e, "output already spent") ||
			strings.Contains(e, chain.ErrInsufficientFee.Error()) {

			f.Errorf("%v: %v", txLabel, err)
			f.LastActionError = err
			return err
		}
	} else {
		f.Debugf("published htlc timeout sweep with txid: %v",
			timeoutTx.TxHash())

		hash := timeoutTx.TxHash()
		f.loopIn.HtlcTimeoutSweepTxHash = &hash
	}

	return nil
}

// toNonces converts a byte slice to a 66 byte slice.
func toNonces(nonces [][]byte) ([][musig2.PubNonceSize]byte, error) {
	res := make([][musig2.PubNonceSize]byte, 0, len(nonces))
	for _, n := range nonces {
		nonce, err := byteSliceTo66ByteSlice(n)
		if err != nil {
			return nil, err
		}

		res = append(res, nonce)
	}

	return res, nil
}

// byteSliceTo66ByteSlice converts a byte slice to a 66 byte slice.
func byteSliceTo66ByteSlice(b []byte) ([musig2.PubNonceSize]byte, error) {
	if len(b) != musig2.PubNonceSize {
		return [musig2.PubNonceSize]byte{},
			fmt.Errorf("invalid byte slice length")
	}

	var res [musig2.PubNonceSize]byte
	copy(res[:], b)

	return res, nil
}
