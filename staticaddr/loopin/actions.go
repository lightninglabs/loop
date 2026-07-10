package loopin

import (
	"bytes"
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
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/staticutil"
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

	defaultInvoiceCleanupTimeout = 5 * time.Second

	monitorRetryDelay = time.Second
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

	var event fsm.EventType
	invoiceNeedsCleanup := false
	defer func() {
		// If we created the private invoice but failed before persisting the
		// swap, cancel it so retries do not accumulate orphan invoices.
		if !invoiceNeedsCleanup || event != fsm.OnError {
			return
		}

		if err := f.cancelSwapInvoice(); err != nil {
			f.Warnf("unable to clean up invoice for swap %v: %v",
				f.loopIn.SwapHash, err)
		}
	}()

	returnError := func(err error) fsm.EventType {
		event = f.HandleError(err)

		return event
	}

	// Lock the deposits and transition them to the LoopingIn state.
	err := f.cfg.DepositManager.TransitionDeposits(
		ctx, f.loopIn.Deposits, deposit.OnLoopInInitiated,
		deposit.LoopingIn,
	)
	if err != nil {
		err = fmt.Errorf("unable to loop-in deposits: %w", err)

		return returnError(err)
	}

	// Calculate the swap invoice amount. The server needs to pay us the
	// swap amount minus the fees that the server charges for the swap. The
	// swap amount is either the total value of the selected deposits, or
	// the selected amount if a specific amount was requested.
	totalDepositAmount := f.loopIn.TotalDepositAmount()
	swapAmount := totalDepositAmount
	var changeAmount btcutil.Amount
	var hasChange bool
	if f.loopIn.SelectedAmount > 0 {
		swapAmount = f.loopIn.SelectedAmount
		changeAmount = totalDepositAmount - swapAmount
		hasChange = changeAmount > 0 && changeAmount < totalDepositAmount
	}
	swapInvoiceAmt := swapAmount - f.loopIn.QuotedSwapFee

	var changeOutput *swapserverrpc.StaticAddressChangeOutput
	if hasChange {
		changeAmount := f.loopIn.ExpectedChangeAmount()
		f.loopIn.ChangeAddressParams, err =
			f.cfg.AddressManager.NewChangeAddress(ctx)
		if err != nil {
			err = fmt.Errorf("unable to create static address "+
				"change output: %w", err)

			return returnError(err)
		}

		changeOutput, err = staticutil.ChangeOutput(
			f.loopIn.ChangeAddressParams, changeAmount,
		)
		if err != nil {
			err = fmt.Errorf("unable to prepare static address "+
				"change output: %w", err)

			return returnError(err)
		}
	}

	// Generate random preimage.
	var swapPreimage lntypes.Preimage
	if _, err = rand.Read(swapPreimage[:]); err != nil {
		err = fmt.Errorf("unable to create random swap preimage: %w",
			err)

		return returnError(err)
	}
	f.loopIn.SwapPreimage = swapPreimage
	f.loopIn.SwapHash = swapPreimage.Hash()

	// Derive a client key for the HTLC.
	keyDesc, err := f.cfg.WalletKit.DeriveNextKey(
		ctx, swap.StaticAddressKeyFamily,
	)
	if err != nil {
		err = fmt.Errorf("unable to derive client htlc key: %w", err)

		return returnError(err)
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

		return returnError(err)
	}
	f.loopIn.SwapInvoice = swapInvoice

	// From here until CreateLoopIn succeeds, any error path would otherwise
	// leave behind a live invoice with no persisted swap to recover it.
	invoiceNeedsCleanup = true

	f.loopIn.ProtocolVersion = version.AddressProtocolVersion(
		version.CurrentRPCProtocolVersion(),
	)

	depositClientPubkeys, err := staticutil.DepositClientPubkeys(
		f.loopIn.Deposits,
	)
	if err != nil {
		err = fmt.Errorf("unable to prepare static address input "+
			"proofs: %w", err)

		return returnError(err)
	}

	loopInReq := &swapserverrpc.ServerStaticAddressLoopInRequest{
		SwapHash:               f.loopIn.SwapHash[:],
		DepositOutpoints:       f.loopIn.DepositOutpoints,
		Amount:                 uint64(f.loopIn.SelectedAmount),
		HtlcClientPubKey:       f.loopIn.ClientPubkey.SerializeCompressed(),
		SwapInvoice:            f.loopIn.SwapInvoice,
		ProtocolVersion:        version.CurrentRPCProtocolVersion(),
		UserAgent:              loop.UserAgent(f.loopIn.Initiator),
		PaymentTimeoutSeconds:  f.loopIn.PaymentTimeoutSeconds,
		Fast:                   f.loopIn.Fast,
		DepositToClientPubkeys: depositClientPubkeys,
		ChangeOutput:           changeOutput,
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

		return returnError(err)
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

		return returnError(err)
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

		return returnError(err)
	}

	f.loopIn.HtlcCltvExpiry = loopInResp.HtlcExpiry
	f.htlcServerNonces, err = toNonces(loopInResp.StandardHtlcInfo.Nonces)
	if err != nil {
		pushEmptySigs()
		err = fmt.Errorf("unable to convert server nonces: %w", err)

		return returnError(err)
	}
	f.htlcServerNoncesHighFee, err = toNonces(
		loopInResp.HighFeeHtlcInfo.Nonces,
	)
	if err != nil {
		pushEmptySigs()

		return returnError(err)
	}
	f.htlcServerNoncesExtremelyHighFee, err = toNonces(
		loopInResp.ExtremeFeeHtlcInfo.Nonces,
	)
	if err != nil {
		pushEmptySigs()

		return returnError(err)
	}

	// We need to defend against the server setting high fees for the htlc
	// tx since we might have to sweep the timeout path. We maximally allow
	// a configured percentage of the swap value to be spent on fees.
	amt := float64(swapAmount)
	maxHtlcTxFee := btcutil.Amount(amt *
		f.cfg.MaxStaticAddrHtlcFeePercentage)

	maxHtlcTxBackupFee := btcutil.Amount(amt *
		f.cfg.MaxStaticAddrHtlcBackupFeePercentage)

	htlcWeight := f.loopIn.htlcWeight(hasChange)
	feeRate := chainfee.SatPerKWeight(loopInResp.StandardHtlcInfo.FeeRate)
	fee := feeRate.FeeForWeight(htlcWeight)
	highFeeRate := chainfee.SatPerKWeight(loopInResp.HighFeeHtlcInfo.FeeRate)
	highFee := highFeeRate.FeeForWeight(htlcWeight)
	extremelyHighFeeRate := chainfee.SatPerKWeight(
		loopInResp.ExtremeFeeHtlcInfo.FeeRate,
	)
	extremelyHighFee := extremelyHighFeeRate.FeeForWeight(htlcWeight)

	f.Debugf("htlc fee validation: "+
		"deposit_count=%d, total_deposit=%v, "+
		"swap_amount=%v, change_amount=%v, has_change=%v, "+
		"htlc_weight=%v, standard_fee_rate=%v, standard_fee=%v, "+
		"high_fee_rate=%v, high_fee=%v, extreme_fee_rate=%v, "+
		"extreme_fee=%v, max_fee=%v, max_backup_fee=%v",
		len(f.loopIn.Deposits), totalDepositAmount, swapAmount,
		changeAmount, hasChange, htlcWeight, feeRate, fee, highFeeRate,
		highFee, extremelyHighFeeRate, extremelyHighFee, maxHtlcTxFee,
		maxHtlcTxBackupFee)

	if fee > maxHtlcTxFee {
		// Abort the swap by pushing empty sigs to the server.
		pushEmptySigs()

		f.Errorf("server standard htlc tx fee is higher than the "+
			"configured allowed maximum: %v > %v "+
			"(fee_rate=%v, weight=%v)",
			fee, maxHtlcTxFee, feeRate, htlcWeight)

		return returnError(ErrFeeTooHigh)
	}
	f.loopIn.HtlcTxFeeRate = feeRate

	if highFee > maxHtlcTxBackupFee {
		// Abort the swap by pushing empty sigs to the server.
		pushEmptySigs()

		f.Errorf("server high-fee htlc backup tx fee is higher "+
			"than the configured allowed maximum: %v > %v "+
			"(fee_rate=%v, weight=%v)",
			highFee, maxHtlcTxBackupFee, highFeeRate, htlcWeight)

		return returnError(ErrFeeTooHigh)
	}
	f.loopIn.HtlcTxHighFeeRate = highFeeRate

	if extremelyHighFee > maxHtlcTxBackupFee {
		// Abort the swap by pushing empty sigs to the server.
		pushEmptySigs()

		f.Errorf("server extreme-fee htlc backup tx fee is "+
			"higher than the configured allowed maximum: %v > %v "+
			"(fee_rate=%v, weight=%v)",
			extremelyHighFee, maxHtlcTxBackupFee,
			extremelyHighFeeRate, htlcWeight)

		return returnError(ErrFeeTooHigh)
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

		return returnError(err)
	}
	f.loopIn.HtlcTimeoutSweepAddress = sweepAddress

	// Once the htlc tx is initiated, we store the loop-in in the database.
	err = f.cfg.Store.CreateLoopIn(ctx, f.loopIn)
	if err != nil {
		pushEmptySigs()
		err = fmt.Errorf("unable to store loop-in in db: %w", err)

		return returnError(err)
	}

	// Once the swap is stored, restart/recovery code owns invoice lifecycle.
	invoiceNeedsCleanup = false

	event = OnHtlcInitiated

	return event
}

// cancelSwapInvoice cancels the current swap invoice using a detached,
// timeout-limited context. Callers that must not proceed while the invoice may
// still be payable can use the returned error to retry.
func (f *FSM) cancelSwapInvoice() error {
	if f.loopIn.SwapHash == (lntypes.Hash{}) {
		return nil
	}

	cleanupCtx, cancel := context.WithTimeout(
		context.Background(), defaultInvoiceCleanupTimeout,
	)
	defer cancel()

	err := f.cfg.InvoicesClient.CancelInvoice(cleanupCtx, f.loopIn.SwapHash)
	return err
}

// handleInvoiceUpdate applies the monitor state's invoice-update semantics and
// reports whether the update produced a terminal event.
func (f *FSM) handleInvoiceUpdate(update lndclient.InvoiceUpdate) (
	fsm.EventType, bool) {

	switch update.State {
	case invoices.ContractOpen:
		return fsm.NoOp, false

	case invoices.ContractAccepted:
		return fsm.NoOp, false

	case invoices.ContractSettled:
		f.Debugf("received off-chain payment update %v", update.State)
		return OnPaymentReceived, true

	case invoices.ContractCanceled:
		// If the invoice was canceled we only log here since we still need
		// to monitor until the htlc timed out.
		log.Warnf("invoice for swap hash %v canceled", f.loopIn.SwapHash)
		return fsm.NoOp, false

	default:
		// An unknown state is not evidence that the invoice can no longer
		// settle. Keep monitoring rather than leaving the deposits available
		// for reuse.
		f.Warnf("unexpected invoice state %v for swap hash %v",
			update.State, f.loopIn.SwapHash)

		return fsm.NoOp, false
	}
}

// selectedDepositConfirmationHeights returns current confirmation heights for
// the original deposit outpoints selected by this loop-in.
func selectedDepositConfirmationHeights(
	loopIn *StaticAddressLoopIn) map[string]int64 {

	confirmations := make(map[string]int64, len(loopIn.Deposits))
	outpoints := make(map[string]struct{}, len(loopIn.DepositOutpoints))
	for _, outpoint := range loopIn.DepositOutpoints {
		outpoints[outpoint] = struct{}{}
	}

	for _, d := range loopIn.Deposits {
		if d == nil {
			continue
		}

		outpoint := d.OutPoint.String()
		confirmationHeight := d.GetConfirmationHeight()

		if _, ok := outpoints[outpoint]; !ok {
			continue
		}

		confirmations[outpoint] = confirmationHeight
	}

	return confirmations
}

// refreshSelectedDeposits reloads the loop-in's selected deposits from the
// deposit manager/store so recovery does not rely on stale deposit snapshots.
func (f *FSM) refreshSelectedDeposits(ctx context.Context) error {
	if f.cfg.DepositManager == nil || len(f.loopIn.DepositOutpoints) == 0 {
		return nil
	}

	err := f.cfg.DepositManager.EnsureDepositsFresh(ctx)
	if err != nil {
		return fmt.Errorf("unable to refresh deposit wallet view: %w", err)
	}

	const ignoreUnknownOutpoints = false
	deposits, err := f.cfg.DepositManager.DepositsForOutpoints(
		ctx, f.loopIn.DepositOutpoints, ignoreUnknownOutpoints,
	)
	if err != nil {
		return err
	}

	if len(deposits) != len(f.loopIn.DepositOutpoints) {
		return fmt.Errorf("expected %d selected deposits, got %d",
			len(f.loopIn.DepositOutpoints), len(deposits))
	}

	f.loopIn.Deposits = deposits

	return nil
}

// legacyMinConfsReached returns true once every original deposit is confirmed
// and the youngest original deposit has reached the legacy confirmation target.
func legacyMinConfsReached(outpoints []string,
	confirmationHeights map[string]int64, currentHeight int32) bool {

	if currentHeight <= 0 || len(outpoints) == 0 {
		return false
	}

	youngestConfirmation := int64(0)
	for _, outpoint := range outpoints {
		confirmationHeight, ok := confirmationHeights[outpoint]
		if !ok || confirmationHeight <= 0 {
			return false
		}

		if confirmationHeight > youngestConfirmation {
			youngestConfirmation = confirmationHeight
		}
	}

	return int64(currentHeight) >= youngestConfirmation+deposit.MinConfs-1
}

// shouldStartLegacyConfirmationFallback reports whether the local MinConfs
// payment deadline fallback should be armed at the current block height.
//
// The primary path starts the deadline from a server risk-accepted notification.
// This fallback preserves the legacy client-side MinConfs behavior when no risk
// decision has been observed locally: once every original deposit reaches
// MinConfs, the client treats that as enough confirmation-risk clearance to
// start the payment window. The selected deposits are refreshed first so
// recovered swaps do not depend on stale in-memory deposit snapshots.
func (f *FSM) shouldStartLegacyConfirmationFallback(ctx context.Context,
	currentHeight int32) bool {

	err := f.refreshSelectedDeposits(ctx)
	if err != nil {
		f.Warnf("unable to refresh selected deposits for legacy "+
			"confirmation fallback: %v", err)

		return false
	}

	depositConfirmationHeights := selectedDepositConfirmationHeights(
		f.loopIn,
	)

	return legacyMinConfsReached(
		f.loopIn.DepositOutpoints, depositConfirmationHeights,
		currentHeight,
	)
}

// originalDepositOutpointUnavailable checks the original selected deposit
// outpoints against the chain backend's UTXO view.
func (f *FSM) originalDepositOutpointUnavailable(ctx context.Context) (
	bool, error) {

	if f.cfg.TxOutChecker == nil {
		return false, nil
	}

	if len(f.loopIn.DepositOutpoints) == 0 {
		return false, nil
	}

	outpoints := make([]wire.OutPoint, len(f.loopIn.DepositOutpoints))
	for i, outpointStr := range f.loopIn.DepositOutpoints {
		outpoint, err := wire.NewOutPointFromString(outpointStr)
		if err != nil {
			return false, fmt.Errorf("invalid deposit outpoint %q: %w",
				outpointStr, err)
		}

		outpoints[i] = *outpoint
	}

	txOuts, err := f.cfg.TxOutChecker.GetTxOuts(ctx, outpoints)
	if err != nil {
		return false, fmt.Errorf("unable to get txouts: %w", err)
	}

	for _, outpoint := range outpoints {
		if txOuts[outpoint] == nil {
			return true, nil
		}
	}

	return false, nil
}

// SignHtlcTxAction is called if the htlc was initialized and the server
// provided the necessary information to construct the htlc tx. We sign the htlc
// tx and send the signatures to the server.
func (f *FSM) SignHtlcTxAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	var err error

	outpointUnavailable, err := f.originalDepositOutpointUnavailable(ctx)
	if err != nil {
		return f.HandleError(err)
	}
	if outpointUnavailable {
		err = errors.New("original deposit outpoint no longer available")
		f.Warnf("%v, canceling swap invoice", err)
		if cancelErr := f.cancelSwapInvoice(); cancelErr != nil {
			f.Warnf("unable to cancel invoice for swap %v: %v",
				f.loopIn.SwapHash, cancelErr)
		}

		return f.HandleError(err)
	}

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

	err = f.checkDepositsAvailable(ctx)
	if err != nil {
		return f.HandleError(err)
	}

	// Create a musig2 session for each deposit and different htlc tx fee
	// rates.
	createSession := staticutil.CreateMusig2Sessions
	htlcSessions, clientHtlcNonces, err := createSession(
		ctx, f.cfg.Signer, f.loopIn.Deposits,
	)
	if err != nil {
		err = fmt.Errorf("unable to create musig2 sessions: %w", err)

		return f.HandleError(err)
	}
	defer f.cleanUpSessions(ctx, htlcSessions)

	htlcSessionsHighFee, highFeeNonces, err := createSession(
		ctx, f.cfg.Signer, f.loopIn.Deposits,
	)
	if err != nil {
		return f.HandleError(err)
	}
	defer f.cleanUpSessions(ctx, htlcSessionsHighFee)

	htlcSessionsExtremelyHighFee, extremelyHighNonces, err := createSession(
		ctx, f.cfg.Signer, f.loopIn.Deposits,
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

// checkDepositsAvailable verifies that all loop-in deposits are still available
// before the client signs the HTLC transaction.
func (f *FSM) checkDepositsAvailable(ctx context.Context) error {
	outpoints, err := f.validateSigningDepositOutpoints()
	if err != nil {
		return err
	}

	if f.cfg.TxOutChecker == nil {
		return nil
	}

	txOuts, err := f.cfg.TxOutChecker.GetTxOuts(ctx, outpoints)
	if err != nil {
		return fmt.Errorf("unable to check deposits: %w", err)
	}

	for _, outpoint := range outpoints {
		if txOuts[outpoint] == nil {
			return fmt.Errorf("deposit %v is no longer available",
				outpoint)
		}
	}

	return nil
}

// validateSigningDepositOutpoints verifies that the current deposit rows match
// the server-side outpoint snapshot before signing the HTLC transaction.
func (f *FSM) validateSigningDepositOutpoints() ([]wire.OutPoint, error) {
	currentOutpoints := f.loopIn.Outpoints()
	if len(f.loopIn.DepositOutpoints) == 0 {
		return currentOutpoints, nil
	}

	if len(f.loopIn.DepositOutpoints) != len(currentOutpoints) {
		return nil, fmt.Errorf("deposit outpoint snapshot has %d "+
			"outpoints, current deposits have %d",
			len(f.loopIn.DepositOutpoints), len(currentOutpoints))
	}

	snapshotOutpoints := make(
		[]wire.OutPoint, len(f.loopIn.DepositOutpoints),
	)
	for i, snapshot := range f.loopIn.DepositOutpoints {
		outpoint, err := wire.NewOutPointFromString(snapshot)
		if err != nil {
			return nil, fmt.Errorf("unable to parse deposit "+
				"outpoint snapshot %q: %w", snapshot, err)
		}

		snapshotOutpoints[i] = *outpoint
		if *outpoint != currentOutpoints[i] {
			return nil, fmt.Errorf("deposit outpoint snapshot "+
				"mismatch at index %d: snapshot %v, "+
				"current %v", i, outpoint, currentOutpoints[i])
		}
	}

	return snapshotOutpoints, nil
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

	retryMonitor := func(err error) fsm.EventType {
		f.Errorf("monitoring failed: %v, retrying", err)

		invoice, lookupErr := f.cfg.LndClient.LookupInvoice(
			ctx, f.loopIn.SwapHash,
		)
		if lookupErr == nil && invoice.State == invoices.ContractSettled {
			return OnPaymentReceived
		}

		select {
		case <-time.After(monitorRetryDelay):
			return OnRecover

		case <-ctx.Done():
			return fsm.NoOp
		}
	}

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
		if ctx.Err() != nil {
			return fsm.NoOp
		}

		err = fmt.Errorf("unable to subscribe to swap "+
			"invoice: %w", err)

		return retryMonitor(err)
	}

	htlc, err := f.loopIn.getHtlc(f.cfg.ChainParams)
	if err != nil {
		err = fmt.Errorf("unable to get htlc: %w", err)

		return retryMonitor(err)
	}

	// Subscribe to htlc tx confirmation.
	reorgChan := make(chan struct{}, 1)
	// registerHtlcConf registers for the HTLC transaction confirmation using
	// the current reorg channel.
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
		if ctx.Err() != nil {
			return fsm.NoOp
		}

		err = fmt.Errorf("unable to monitor htlc tx confirmation: %w",
			err)

		return retryMonitor(err)
	}

	// Subscribe to new blocks.
	registerBlocks := f.cfg.ChainNotifier.RegisterBlockEpochNtfn
	blockChan, blockChanErr, err := registerBlocks(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return fsm.NoOp
		}

		err = fmt.Errorf("unable to subscribe to new blocks: %w", err)

		return retryMonitor(err)
	}

	// The watcher keeps notification normalization and timestamp restoration
	// outside of the swap-state handling below.
	riskWatcher := newConfirmationRiskWatcher(
		f.cfg, f.loopIn.SwapHash, f.Warnf,
	)
	riskUpdateChan, cancelRiskNotificationSubscriptions :=
		riskWatcher.subscribe(ctx)
	defer cancelRiskNotificationSubscriptions()

	// Look up the current invoice state after registering subscriptions so
	// recovery can resume the payment deadline from the latest known state.
	invoice, err := f.cfg.LndClient.LookupInvoice(ctx, f.loopIn.SwapHash)
	if err != nil {
		if ctx.Err() != nil {
			return fsm.NoOp
		}

		// A failed lookup leaves the invoice state unknown. The active
		// subscription can still provide an authoritative update, so keep
		// monitoring and, most importantly, keep the deposits locked.
		f.Warnf("unable to look up invoice by swap hash: %v", err)
		invoice = &lndclient.Invoice{}
	}

	// A settled invoice always takes precedence over a recovered risk
	// rejection or an elapsed payment deadline.
	if invoice.State == invoices.ContractSettled {
		return OnPaymentReceived
	}

	invoiceCanceledForNonPayment := invoice.State == invoices.ContractCanceled
	if invoiceCanceledForNonPayment {
		// If the invoice was canceled previously we end our
		// subscription to invoice updates.
		cancelInvoiceSubscription()
	}

	// Create the swap payment timeout timer after the server confirms
	// confirmation risk was accepted. If a server does not support risk
	// notifications, fall back after the legacy deposit confirmation depth.
	var (
		deadlineChan    <-chan time.Time
		deadlineTimer   *time.Timer
		deadlineStarted bool
	)
	// Stop the payment deadline timer when leaving the monitor action.
	defer func() {
		if deadlineTimer != nil {
			deadlineTimer.Stop()
		}
	}()

	// depositsInState reports whether all selected deposits are currently
	// in the requested state.
	depositsInState := func(state fsm.StateType) bool {
		if len(f.loopIn.Deposits) == 0 {
			return false
		}

		for _, d := range f.loopIn.Deposits {
			if d == nil {
				return false
			}

			if !d.IsInState(state) {
				return false
			}
		}

		return true
	}

	// startPaymentDeadline arms the server payment timeout from the decision
	// time when one is available.
	startPaymentDeadline := func(reason string, startedAt time.Time) {
		if deadlineStarted || invoice.State == invoices.ContractCanceled {
			return
		}

		timeout := f.loopIn.PaymentTimeoutDuration()
		if !startedAt.IsZero() {
			timeout -= time.Since(startedAt)
			if timeout < 0 {
				timeout = 0
			}
		}

		f.Infof("starting payment deadline after %s", reason)
		deadlineTimer = time.NewTimer(timeout)
		deadlineChan = deadlineTimer.C
		deadlineStarted = true
	}

	depositsLockedForHtlcTimeout := depositsInState(
		deposit.SweepHtlcTimeout,
	)

	// transitionDepositsToHtlcTimeout locks deposits into timeout sweeping once
	// the HTLC is confirmed and the invoice cannot be paid.
	transitionDepositsToHtlcTimeout := func(reason string) {
		if depositsLockedForHtlcTimeout ||
			depositsInState(deposit.SweepHtlcTimeout) {

			depositsLockedForHtlcTimeout = true
			return
		}

		err = f.cfg.DepositManager.TransitionDeposits(
			ctx, f.loopIn.Deposits,
			deposit.OnSweepingHtlcTimeout,
			deposit.SweepHtlcTimeout,
		)
		if err != nil {
			f.Errorf("unable to transition deposits to the htlc "+
				"timeout sweeping state after %s: %v",
				reason, err)

			return
		}

		depositsLockedForHtlcTimeout = true
	}

	// startLegacyFallback starts the payment deadline once the old local
	// minimum-confirmation rule has been satisfied.
	startLegacyFallback := func(reason string, currentHeight int32) {
		if deadlineStarted || invoice.State == invoices.ContractCanceled ||
			f.loopIn.ConfirmationRiskDecision !=
				ConfirmationRiskDecisionNone {

			return
		}

		if f.shouldStartLegacyConfirmationFallback(ctx, currentHeight) {
			decisionTime, ok := riskWatcher.durableDecisionTime(
				ctx, ConfirmationRiskDecisionAccepted,
			)
			if !ok {
				return
			}

			f.loopIn.ConfirmationRiskDecision =
				ConfirmationRiskDecisionAccepted
			f.loopIn.ConfirmationRiskDecisionTime = decisionTime
			startPaymentDeadline(reason, decisionTime)
		}
	}

	// cancelInvoice only marks the invoice canceled after lnd acknowledges
	// the request or the lookup/subscription already observed that state.
	// Failures recover the monitor state without releasing deposits.
	cancelInvoice := func(reason string) (fsm.EventType, bool) {
		if invoice.State != invoices.ContractCanceled {
			f.Errorf("%s, canceling invoice", reason)
			if err := f.cancelSwapInvoice(); err != nil {
				return retryMonitor(err), false
			}
		}

		cancelInvoiceSubscription()
		invoice.State = invoices.ContractCanceled
		invoiceCanceledForNonPayment = true

		return fsm.NoOp, true
	}

	// handleRiskRejected records a server rejection and only exits through
	// the generic error path once the invoice can no longer settle.
	handleRiskRejected := func(reason string,
		decisionTime time.Time) fsm.EventType {

		f.loopIn.ConfirmationRiskDecision =
			ConfirmationRiskDecisionRejected
		f.loopIn.ConfirmationRiskDecisionTime = decisionTime

		event, canceled := cancelInvoice(
			"server rejected confirmation risk wait after " + reason,
		)
		if !canceled {
			return event
		}

		return f.HandleError(fmt.Errorf(
			"server rejected confirmation risk wait after %s", reason,
		))
	}

	switch f.loopIn.ConfirmationRiskDecision {
	case ConfirmationRiskDecisionAccepted:
		startPaymentDeadline(
			"recovered risk accepted notification",
			f.loopIn.ConfirmationRiskDecisionTime,
		)

	case ConfirmationRiskDecisionRejected:
		decisionTime := riskWatcher.decisionTime(
			ctx, ConfirmationRiskDecisionRejected,
		)

		return handleRiskRejected(
			"recovered risk rejection", decisionTime,
		)
	}

	info, err := f.cfg.LndClient.GetInfo(ctx)
	if err != nil {
		f.Warnf("unable to query current height for legacy confirmation "+
			"fallback: %v", err)
	} else {
		startLegacyFallback(
			"legacy confirmation fallback", int32(info.BlockHeight),
		)
	}

	htlcConfirmed := false
	for {
		select {
		case conf := <-htlcConfChan:
			f.Infof("htlc tx confirmed")

			err = f.recordConfirmedHtlc(ctx, conf, htlc.PkScript)
			if err != nil {
				return f.HandleError(err)
			}

			htlcConfirmed = true
			if invoiceCanceledForNonPayment {
				transitionDepositsToHtlcTimeout(
					"htlc confirmation after invoice cancellation",
				)
			}

		case err = <-htlcErrConfChan:
			if ctx.Err() != nil {
				return fsm.NoOp
			}

			f.Errorf("htlc tx conf chan error, re-registering: "+
				"%v", err)

			// A previous confirmation may no longer be valid if the
			// subscription failed, so reset and wait for a fresh one.
			htlcConfirmed = false

			// Re-register for htlc confirmation.
			htlcConfChan, htlcErrConfChan, err = registerHtlcConf()
			if err != nil {
				if ctx.Err() != nil {
					return fsm.NoOp
				}

				err = fmt.Errorf("unable to re-register for "+
					"htlc tx confirmation: %w", err)

				return retryMonitor(err)
			}

		case <-reorgChan:
			// A reorg happened. We invalidate a previous htlc
			// confirmation and re-register for the next
			// confirmation.
			htlcConfirmed = false
			err = f.clearConfirmedHtlc(ctx)
			if err != nil {
				return f.HandleError(err)
			}

			htlcConfChan, htlcErrConfChan, err = registerHtlcConf()
			if err != nil {
				if ctx.Err() != nil {
					return fsm.NoOp
				}

				err = fmt.Errorf("unable to monitor htlc tx "+
					"confirmation: %v", err)

				return retryMonitor(err)
			}

		case <-deadlineChan:
			deadlineChan = nil

			// If the server didn't pay the invoice on time, we cancel
			// it and keep monitoring the htlc tx. Confirmed HTLC
			// deposits remain locked for timeout sweeping.
			event, canceled := cancelInvoice(
				"timeout waiting for invoice to be paid",
			)
			if !canceled {
				return event
			}
			if htlcConfirmed {
				transitionDepositsToHtlcTimeout("payment deadline")
				continue
			}

			err = f.unlockDeposits(ctx)
			if err != nil {
				f.Errorf("unable to unlock deposits after "+
					"payment deadline: %v", err)
			}

		case riskUpdate, ok := <-riskUpdateChan:
			if !ok {
				riskUpdateChan = nil
				continue
			}

			decisionTime := riskWatcher.decisionTime(
				ctx, riskUpdate.decision,
			)
			f.loopIn.ConfirmationRiskDecision = riskUpdate.decision
			f.loopIn.ConfirmationRiskDecisionTime = decisionTime

			switch riskUpdate.decision {
			case ConfirmationRiskDecisionAccepted:
				startPaymentDeadline(
					riskUpdate.reason,
					f.loopIn.ConfirmationRiskDecisionTime,
				)

			case ConfirmationRiskDecisionRejected:
				return handleRiskRejected(
					riskUpdate.reason, decisionTime,
				)
			}

		case currentHeight := <-blockChan:
			startLegacyFallback(
				"legacy confirmation fallback", currentHeight,
			)

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
			event, canceled := cancelInvoice("htlc timed out")
			if !canceled {
				return event
			}

			if !htlcConfirmed {
				f.Infof("swap timed out, htlc not confirmed")

				// If the htlc hasn't confirmed but the timeout
				// path opened up, and we didn't receive the
				// swap payment, we consider the swap attempt to
				// be failed. Now that the invoice is canceled and
				// the HTLC can no longer confirm, its deposits can be
				// made available again.
				err = f.unlockDeposits(ctx)
				if err != nil {
					f.Errorf("unable to unlock deposits after "+
						"htlc timeout: %v", err)
				}

				return OnSwapTimedOut
			}

			// If the htlc has confirmed and the timeout path has
			// opened up we sweep the funds back to us.
			transitionDepositsToHtlcTimeout("htlc timeout")

			return OnSweepHtlcTimeout

		case err = <-blockChanErr:
			if ctx.Err() != nil {
				return fsm.NoOp
			}

			f.Errorf("block subscription error: %v", err)

			return retryMonitor(err)

		case update, ok := <-invoiceUpdateChan:
			if !ok {
				if !invoiceCanceledForNonPayment {
					return retryMonitor(errors.New(
						"invoice update subscription closed",
					))
				}

				invoiceUpdateChan = nil
				continue
			}

			if event, done := f.handleInvoiceUpdate(update); done {
				return event
			}

			invoice.State = update.State
			if update.State == invoices.ContractCanceled {
				invoiceCanceledForNonPayment = true
			}

		case err, ok := <-invoiceErrChan:
			if !ok {
				if !invoiceCanceledForNonPayment {
					return retryMonitor(errors.New(
						"invoice error subscription closed",
					))
				}

				invoiceErrChan = nil
				continue
			}

			if ctx.Err() != nil {
				return fsm.NoOp
			}

			return retryMonitor(fmt.Errorf(
				"invoice subscription error: %w", err,
			))

		case <-ctx.Done():
			return fsm.NoOp
		}
	}
}

func (f *FSM) recordConfirmedHtlc(ctx context.Context,
	conf *chainntnfs.TxConfirmation, htlcPkScript []byte) error {

	if conf == nil || conf.Tx == nil {
		return errors.New("htlc confirmation missing transaction")
	}
	if f.cfg.Store == nil {
		return errors.New("missing static address loop-in store")
	}

	tx := conf.Tx
	txHash := tx.TxHash()
	for idx, txOut := range tx.TxOut {
		if !bytes.Equal(txOut.PkScript, htlcPkScript) {
			continue
		}

		f.loopIn.HtlcTxHash = &txHash
		f.loopIn.HtlcOutputIndex = uint32(idx)
		f.loopIn.HtlcOutputValue = btcutil.Amount(txOut.Value)

		return f.cfg.Store.UpdateLoopIn(ctx, f.loopIn)
	}

	return fmt.Errorf("confirmed htlc tx %v missing expected htlc "+
		"output", txHash)
}

func (f *FSM) clearConfirmedHtlc(ctx context.Context) error {
	if f.loopIn.HtlcTxHash == nil && f.loopIn.HtlcOutputIndex == 0 &&
		f.loopIn.HtlcOutputValue == 0 {

		return nil
	}
	if f.cfg.Store == nil {
		return errors.New("missing static address loop-in store")
	}

	f.loopIn.HtlcTxHash = nil
	f.loopIn.HtlcOutputIndex = 0
	f.loopIn.HtlcOutputValue = 0

	return f.cfg.Store.UpdateLoopIn(ctx, f.loopIn)
}

// htlcTimeoutSweepRetryDelay is the delay between retries when publishing the
// htlc timeout sweep transaction fails.
const htlcTimeoutSweepRetryDelay = time.Hour

// SweepHtlcTimeoutAction is called if the server published the htlc tx without
// paying the invoice. We wait for the timeout path to open up and sweep the
// funds back to us.
func (f *FSM) SweepHtlcTimeoutAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	for {
		err := f.createAndPublishHtlcTimeoutSweepTx(ctx)
		if err == nil {
			return OnHtlcTimeoutSweepPublished
		}

		f.Errorf("unable to create and publish htlc timeout sweep "+
			"tx: %v, retrying in %v", err, htlcTimeoutSweepRetryDelay)

		select {
		// The context is cancelled when the server is shutting
		// down. Keep the current state so recovery resumes
		// broadcasting attempts after restart.
		case <-ctx.Done():
			return fsm.NoOp

		case <-time.After(htlcTimeoutSweepRetryDelay):
		}
	}
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
		if ctx.Err() != nil {
			return fsm.NoOp
		}

		err = fmt.Errorf("unable to register to the htlc timeout "+
			"sweep tx: %w", err)

		return f.HandleError(err)
	}

	for {
		select {
		case err := <-errChan:
			if ctx.Err() != nil {
				return fsm.NoOp
			}

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
			return fsm.NoOp
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

	if err := f.cancelSwapInvoice(); err != nil {
		f.Warnf("unable to cancel invoice for swap %v: %v",
			f.loopIn.SwapHash, err)
	}

	err := f.unlockDeposits(ctx)
	if err != nil {
		return f.HandleError(err)
	}

	return fsm.OnError
}

func (f *FSM) unlockDeposits(ctx context.Context) error {
	err := f.cfg.DepositManager.TransitionDeposits(
		ctx, f.loopIn.Deposits, fsm.OnError, deposit.Deposited,
	)
	if err != nil {
		return fmt.Errorf("unable to unlock deposits: %w", err)
	}

	return nil
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
