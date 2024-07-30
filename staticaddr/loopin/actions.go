package loopin

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	loop_rpc "github.com/lightninglabs/loop/swapserverrpc"
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
)

// InitHtlcAction is executed if all loop-in information has been validated. We
// assemble a loop-in request and send it to the server.
func (f *FSM) InitHtlcAction(_ fsm.EventContext) fsm.EventType {
	// Lock the deposits and transition them to the LoopingIn state.
	err := f.cfg.DepositManager.TransitionDeposits(
		f.loopIn.Deposits, deposit.OnLoopinInitiated, deposit.LoopingIn,
	)
	if err != nil {
		return f.HandleError(err)
	}

	// Calculate the swap invoice amount. The pre-pay is added which
	// effectively forces the server to pay us back our prepayment on a
	// successful swap.
	swapInvoiceAmt := f.loopIn.TotalDepositAmount() - f.loopIn.QuotedSwapFee

	// Generate random preimage.
	var swapPreimage lntypes.Preimage
	if _, err := rand.Read(swapPreimage[:]); err != nil {
		log.Error("Cannot generate preimage")
	}
	f.loopIn.SwapPreimage = swapPreimage
	f.loopIn.SwapHash = sha256.Sum256(swapPreimage[:])

	// Derive a client key for the HTLC.
	keyDesc, err := f.cfg.WalletKit.DeriveNextKey(
		f.ctx, swap.StaticAddressKeyFamily,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.ClientPubkey = keyDesc.PubKey
	f.loopIn.HtlcKeyLocator = keyDesc.KeyLocator

	var clientKey [33]byte
	copy(clientKey[:], keyDesc.PubKey.SerializeCompressed())

	// Create the swap invoice in lnd.
	_, swapInvoice, err := f.cfg.LndClient.AddInvoice(
		f.ctx, &invoicesrpc.AddInvoiceData{
			Preimage:   &swapPreimage,
			Value:      lnwire.NewMSatFromSatoshis(swapInvoiceAmt),
			Memo:       "static address loop-in",
			Expiry:     3600 * 24 * 365,
			RouteHints: f.loopIn.RouteHints,
		},
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.SwapInvoice = swapInvoice

	f.loopIn.ProtocolVersion = version.AddressProtocolVersion(
		version.CurrentRPCProtocolVersion(),
	)

	loopInReq := &loop_rpc.ServerStaticAddressLoopInRequest{
		SwapHash:         f.loopIn.SwapHash[:],
		DepositOutpoints: f.loopIn.DepositOutpoints,
		HtlcClientKey:    f.loopIn.ClientPubkey.SerializeCompressed(),
		SwapInvoice:      f.loopIn.SwapInvoice,
		ProtocolVersion:  version.CurrentRPCProtocolVersion(),
		UserAgent:        f.loopIn.Initiator,
	}
	if f.loopIn.LastHop != nil {
		loopInReq.LastHop = f.loopIn.LastHop
	}

	loopInResp, err := f.cfg.StaticAddressServerClient.ServerStaticAddressLoopIn( //nolint:lll
		f.ctx, loopInReq,
	)
	if err != nil {
		return f.HandleError(err)
	}

	serverPubkey, err := btcec.ParsePubKey(loopInResp.HtlcServerKey)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.ServerPubkey = serverPubkey

	// TODO: add cltv validity check.

	f.loopIn.HtlcCltvExpiry = loopInResp.HtlcExpiry
	f.htlcServerNonces, err = toNonces(loopInResp.HtlcServerNonces)
	if err != nil {
		return f.HandleError(err)
	}

	f.loopIn.HtlcTxFeeRate = chainfee.SatPerKWeight(loopInResp.HtlcFeeRate)

	// Derive the sweep address for the htlc timeout sweep tx.
	sweepAddress, err := f.cfg.WalletKit.NextAddr(
		f.ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.HtlcTimeoutSweepAddress = sweepAddress

	return OnHtlcInitiated
}

// SignHtlcTxAction is called if the htlc was initialized and the server
// provided the necessary information to construct the htlc tx. We sign the htlc
// tx and send the signatures to the server.
func (f *FSM) SignHtlcTxAction(_ fsm.EventContext) fsm.EventType {
	var err error

	f.loopIn.AddressParams, err = f.cfg.AddressManager.GetStaticAddressParameters( //nolint:lll
		f.ctx,
	)
	if err != nil {
		return f.HandleError(err)
	}

	f.loopIn.Address, err = f.cfg.AddressManager.GetStaticAddress(f.ctx)
	if err != nil {
		return f.HandleError(err)
	}

	// Create a musig2 session for each deposit.
	htlcSessions, clientHtlcNonces, err := f.loopIn.createMusig2Sessions(
		f.ctx, f.cfg.Signer,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.htlcMusig2Sessions = htlcSessions
	f.htlcClientNonces, err = toNonces(clientHtlcNonces)
	if err != nil {
		return f.HandleError(err)
	}

	htlcTx, err := f.loopIn.createHtlcTx(f.cfg.ChainParams)
	if err != nil {
		return f.HandleError(err)
	}

	// Next we'll get our htlc tx signatures.
	htlcSigs, err := f.loopIn.signMusig2Tx(
		f.ctx, htlcTx, f.cfg.Signer, htlcSessions, f.htlcServerNonces,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.htlcClientSigs = htlcSigs

	// Push htlc tx sigs to server.
	pushHtlcReq := &loop_rpc.PushStaticAddressHtlcSigsRequest{
		SwapHash:         f.loopIn.SwapHash[:],
		HtlcClientNonces: clientHtlcNonces,
		HtlcClientSigs:   htlcSigs,
	}
	sigPushResp, err := f.cfg.StaticAddressServerClient.PushStaticAddressHtlcSigs( //nolint:lll
		f.ctx, pushHtlcReq,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.HtlcTx = htlcTx

	// From here on we need to monitor for the htlc tx hitting the chain
	// until the invoice is settled because the server can now publish the
	// htlc tx without paying the invoice. In this case we need to wait till
	// the htlc times out and then sweep it back to us.
	// If the following calls produce an error we only log them and continue
	// with monitoring the invoice settlement and listen for the htlc tx to
	// be mined. Otherwise, if we returned on an error in the following, we
	// wouldn't be able to monitor the htlc tx.
	// Also, if the following code fails we couldn't sign the server's
	// sweepless sweep, which wouldn't hinder the swap to succeed.
	f.sweeplessServerNonces, err = toNonces(
		sigPushResp.SweeplessServerNonces,
	)
	if err != nil {
		log.Warnf("unable to convert server nonces: %v", err)
	}
	sweeplessAddress, err := btcutil.DecodeAddress(
		sigPushResp.SweeplessSweepAddr, f.cfg.ChainParams,
	)
	if err != nil {
		log.Warnf("unable to decode sweep address: %v", err)
	}
	f.loopIn.SweeplessSweepAddress = sweeplessAddress
	f.loopIn.SweeplessSweepFeeRate = chainfee.SatPerKWeight(
		sigPushResp.SweeplessFeeRate,
	)

	return OnHtlcTxSigned
}

// MonitorInvoiceAndHtlcTxAction is called after the htlc tx has been signed by
// us. The server from here on has the ability to publish the htlc tx. If the
// server publishes the htlc tx without paying the invoice, we have to monitor
// for the timeout path and sweep the funds back to us. If, while waiting for
// the htlc timeout, our invoice gets paid, the swap is considered successful,
// and we can stop monitoring the htlc confirmation and continue to sign the
// sweepless sweep.
func (f *FSM) MonitorInvoiceAndHtlcTxAction(_ fsm.EventContext) fsm.EventType {
	// Subscribe to swap invoice settlement.
	invoiceUpdateChan, invoiceErrChan, err := f.cfg.InvoicesClient.SubscribeSingleInvoice( //nolint:lll
		f.ctx, f.loopIn.SwapHash,
	)
	if err != nil {
		return f.HandleError(err)
	}

	htlc, err := f.loopIn.getHtlc(f.cfg.ChainParams)
	if err != nil {
		return f.HandleError(err)
	}

	// Subscribe to htlc tx confirmation.
	htlcConfChan, htlcErrConfChan, err := f.cfg.ChainNotifier.RegisterConfirmationsNtfn( //nolint:lll
		f.ctx, nil, htlc.PkScript, defaultConfTarget,
		int32(f.loopIn.InitiationHeight),
	)
	if err != nil {
		return f.HandleError(err)
	}

	// Subscribe to new blocks.
	blockChan, blockChanErr, err := f.cfg.ChainNotifier.RegisterBlockEpochNtfn(f.ctx) //nolint:lll
	if err != nil {
		return f.HandleError(err)
	}

	htlcConfirmed := false
	for {
		select {
		case <-htlcConfChan:
			htlcConfirmed = true

		case err = <-htlcErrConfChan:
			return f.HandleError(err)

		case currentHeight := <-blockChan:
			if !htlcConfirmed {
				continue
			}

			if f.loopIn.isHtlcTimedOut(currentHeight) {
				// Unlock the deposits and transition them to
				// the LoopedIn state.
				err = f.cfg.DepositManager.TransitionDeposits(
					f.loopIn.Deposits,
					deposit.OnSweepingHtlcTimout,
					deposit.SweepHtlcTimout,
				)
				if err != nil {
					return f.HandleError(err)
				}
				return OnSweepHtlcTimout
			}

		case err = <-blockChanErr:
			return f.HandleError(err)

		case err = <-invoiceErrChan:
			return f.HandleError(err)

		case update := <-invoiceUpdateChan:
			switch update.State {
			case invoices.ContractOpen:
			case invoices.ContractAccepted:
			case invoices.ContractSettled:
				f.Debugf("received off-chain payment: %v "+
					"update: %v", f.loopIn.SwapHash,
					update.State)

				return OnPaymentReceived

			case invoices.ContractCanceled:
				return f.HandleError(fmt.Errorf("invoice " +
					"canceled"))

			default:
				return f.HandleError(fmt.Errorf("unexpected "+
					"invoice state: %v", update.State))
			}

		case <-time.After(20 * time.Second):
			// TODO: pack timeout into a config.
			log.Errorf("timeout waiting for invoice to be paid, "+
				"canceling invoice: %v", f.loopIn.SwapHash)

			err = f.cfg.InvoicesClient.CancelInvoice(
				f.ctx, f.loopIn.SwapHash,
			)
			if err != nil {
				log.Warnf("unable to cancel invoice for "+
					"swap hash: %v %v", f.loopIn.SwapHash,
					err)
			}

			return OnPaymentTimedOut

		case <-f.ctx.Done():
			return f.HandleError(f.ctx.Err())
		}
	}
}

// SweepHtlcTimeoutAction is called if the server published the htlc tx without
// paying the invoice. We wait for the timeout path to open up and sweep the
// funds back to us.
func (f *FSM) SweepHtlcTimeoutAction(_ fsm.EventContext) fsm.EventType {
	for {
		err := f.createAndPublishHtlcTimoutSweepTx()
		if err == nil {
			break
		}

		// TODO: pack into a timeout config.
		<-time.After(1 * time.Hour)
	}

	return OnHtlcTimeoutSweepPublished
}

// MonitorHtlcTimeoutSweepAction is called after the htlc timeout sweep tx has
// been published. We monitor the confirmation of the htlc timeout sweep tx and
// finalize the deposits once swept.
func (f *FSM) MonitorHtlcTimeoutSweepAction(_ fsm.EventContext) fsm.EventType {
	htlcTimeoutTxid := f.loopIn.HtlcTimeoutSweepTx.TxHash()
	timeoutSweepPkScript, err := txscript.PayToAddrScript(
		f.loopIn.HtlcTimeoutSweepAddress,
	)
	if err != nil {
		return f.HandleError(err)
	}

	htlcTimeoutTxidChan, errChan, err := f.cfg.ChainNotifier.RegisterConfirmationsNtfn( //nolint:lll
		f.ctx, &htlcTimeoutTxid, timeoutSweepPkScript,
		defaultConfTarget, int32(f.loopIn.InitiationHeight),
	)
	if err != nil {
		return f.HandleError(err)
	}

	for {
		select {
		case err := <-errChan:
			return f.HandleError(err)

		case <-htlcTimeoutTxidChan:
			err = f.cfg.DepositManager.TransitionDeposits(
				f.loopIn.Deposits,
				deposit.OnHtlcTimeoutSwept,
				deposit.HtlcTimeoutSwept,
			)
			if err != nil {
				return f.HandleError(err)
			}

			return OnHtlcTimeoutSwept

		case <-f.ctx.Done():
			return f.HandleError(f.ctx.Err())
		}
	}
}

// PaymentReceivedAction is called if the invoice was settled. We finalize the
// deposits by transitioning them to the LoopedIn state.
func (f *FSM) PaymentReceivedAction(_ fsm.EventContext) fsm.EventType {
	// Unlock the deposits and transition them to the LoopedIn state.
	err := f.cfg.DepositManager.TransitionDeposits(
		f.loopIn.Deposits, deposit.OnLoopedIn, deposit.LoopedIn,
	)
	if err != nil {
		return f.HandleError(err)
	}

	return OnSignSweeplessSweep
}

// SignSweeplessSweepAction is called if the invoice was settled. We sign the
// servers request to sweep the deposits to the server's sweep address.
func (f *FSM) SignSweeplessSweepAction(_ fsm.EventContext) fsm.EventType {
	// Create a musig2 session for each deposit.
	musig2Sessions, clientNonces, err := f.loopIn.createMusig2Sessions(
		f.ctx, f.cfg.Signer,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.sweeplessMusig2Sessions = musig2Sessions
	f.sweeplessClientNonces, err = toNonces(clientNonces)
	if err != nil {
		return f.HandleError(err)
	}

	// Create the sweepless sweep transaction.
	sweeplessSweepTx, err := f.loopIn.createSweeplessSweepTx()
	if err != nil {
		return f.HandleError(err)
	}

	// Next we'll get our htlc tx signatures.
	sweeplessSigs, err := f.loopIn.signMusig2Tx(
		f.ctx, sweeplessSweepTx, f.cfg.Signer,
		f.sweeplessMusig2Sessions, f.sweeplessServerNonces,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.sweeplessClientSigs = sweeplessSigs

	// Push Htlc sigs to server
	pushSweeplessSigsReq := &loop_rpc.PushStaticAddressSweeplessSigsRequest{ //nolint:lll
		SwapHash:              f.loopIn.SwapHash[:],
		SweeplessClientNonces: fromNonces(f.sweeplessClientNonces),
		SweeplessClientSigs:   f.sweeplessClientSigs,
	}
	_, err = f.cfg.StaticAddressServerClient.PushStaticAddressSweeplessSigs( //nolint:lll
		f.ctx, pushSweeplessSigsReq,
	)
	if err != nil {
		return f.HandleError(err)
	}

	return OnSweeplessSweepSigned
}

// ResetDepositsAction is called if the loop-in failed and its deposits should
// be available in a future loop-in request.
func (f *FSM) ResetDepositsAction(_ fsm.EventContext) fsm.EventType {
	err := f.cfg.DepositManager.TransitionDeposits(
		f.loopIn.Deposits, fsm.OnError, deposit.Deposited,
	)
	if err != nil {
		return f.HandleError(err)
	}

	return fsm.OnError
}

// createAndPublishHtlcTimoutSweepTx creates and publishes the htlc timeout
// sweep transaction.
func (f *FSM) createAndPublishHtlcTimoutSweepTx() error {
	// Get a fee rate.
	feeRate, err := f.cfg.WalletKit.EstimateFeeRate(
		f.ctx, defaultConfTarget,
	)
	if err != nil {
		return err
	}

	getInfo, err := f.cfg.LndClient.GetInfo(f.ctx)
	if err != nil {
		return err
	}

	// Create htlc timeout transaction.
	timeoutTx, err := f.loopIn.createHtlcSweepTx(
		f.ctx, f.cfg.Signer, f.loopIn.HtlcTimeoutSweepAddress, feeRate,
		f.cfg.ChainParams, getInfo.BlockHeight,
	)
	if err != nil {
		return err
	}

	// Broadcast htlc timeout transaction.
	txLabel := fmt.Sprintf("htlc-timeout-sweep-%v",
		f.loopIn.SwapHash)

	err = f.cfg.WalletKit.PublishTransaction(f.ctx, timeoutTx, txLabel)
	if err != nil {
		if !strings.Contains(err.Error(), "output already spent") {
			log.Errorf("%v: %v", txLabel, err)
			f.LastActionError = err
			return err
		}
	} else {
		f.Debugf("published htlc timeout sweep with txid: %v",
			timeoutTx.TxHash())
	}
	f.loopIn.HtlcTimeoutSweepTx = timeoutTx

	return nil
}

// toNonces converts a byte slice to a 66 byte slice.
func toNonces(nonces [][]byte) ([][musig2.PubNonceSize]byte, error) {
	res := make([][musig2.PubNonceSize]byte, 0, len(nonces))
	for _, n := range nonces {
		n := n
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

func fromNonces(nonces [][musig2.PubNonceSize]byte) [][]byte {
	var result [][]byte
	for _, nonce := range nonces {
		temp := make([]byte, musig2.PubNonceSize)
		copy(temp, nonce[:])
		result = append(result, temp)
	}

	return result
}
