package instantout

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	// Define route independent max routing fees. We have currently no way
	// to get a reliable estimate of the routing fees. Best we can do is
	// the minimum routing fees, which is not very indicative.
	maxRoutingFeeBase = btcutil.Amount(10)

	maxRoutingFeeRate = int64(20000)

	// urgentConfTarget is the target number of blocks for the htlc to be
	// confirmed quickly.
	urgentConfTarget = int32(3)

	// normalConfTarget is the target number of blocks for the sweepless
	// sweep to be confirmed.
	normalConfTarget = int32(6)

	// defaultMaxParts is the default maximum number of parts for the swap.
	defaultMaxParts = uint32(5)

	// defaultSendpaymentTimeout is the default timeout for the swap invoice.
	defaultSendpaymentTimeout = time.Minute * 5

	// defaultPollPaymentTime is the default time to poll the server for the
	// payment status.
	defaultPollPaymentTime = time.Second * 15

	// htlcExpiryDelta is the delta in blocks we require between the htlc
	// expiry and reservation expiry.
	htlcExpiryDelta = int32(40)
)

// InitInstantOutCtx contains the context for the InitInstantOutAction.
type InitInstantOutCtx struct {
	cltvExpiry      int32
	reservations    []reservation.ID
	initationHeight int32
	outgoingChanSet loopdb.ChannelSet
	protocolVersion ProtocolVersion
	sweepAddress    btcutil.Address
}

// InitInstantOutAction is the first action that is executed when the instant
// out FSM is started. It will send the instant out request to the server.
func (f *FSM) InitInstantOutAction(eventCtx fsm.EventContext) fsm.EventType {
	initCtx, ok := eventCtx.(*InitInstantOutCtx)
	if !ok {
		return f.HandleError(fsm.ErrInvalidContextType)
	}

	if len(initCtx.reservations) == 0 {
		return f.HandleError(fmt.Errorf("no reservations provided"))
	}

	var (
		reservationAmt uint64
		reservationIds = make([][]byte, 0, len(initCtx.reservations))
		reservations   = make(
			[]*reservation.Reservation, 0, len(initCtx.reservations),
		)
	)

	// The requested amount needs to be full reservation amounts.
	for _, reservationId := range initCtx.reservations {
		resId := reservationId
		res, err := f.cfg.ReservationManager.GetReservation(
			f.ctx, resId,
		)
		if err != nil {
			return f.HandleError(err)
		}

		// Check if the reservation is locked.
		if res.State == reservation.Locked {
			return f.HandleError(fmt.Errorf("reservation %v is "+
				"locked", reservationId))
		}

		reservationAmt += uint64(res.Value)
		reservationIds = append(reservationIds, resId[:])
		reservations = append(reservations, res)

		// Check that the reservation expiry is larger than the cltv
		// expiry of the swap, with an additional delta to allow for
		// preimage reveal.
		if int32(res.Expiry) < initCtx.cltvExpiry+htlcExpiryDelta {
			return f.HandleError(fmt.Errorf("reservation %x has "+
				"expiry %v which is less than the swap expiry %v",
				resId, res.Expiry, initCtx.cltvExpiry+htlcExpiryDelta))
		}
	}

	// Create the preimage for the swap.
	var preimage lntypes.Preimage
	if _, err := rand.Read(preimage[:]); err != nil {
		return f.HandleError(err)
	}

	// Create the keys for the swap.
	keyRes, err := f.cfg.Wallet.DeriveNextKey(f.ctx, KeyFamily)
	if err != nil {
		return f.HandleError(err)
	}

	swapHash := preimage.Hash()

	// Create a high fee rate so that the htlc will be confirmed quickly.
	feeRate, err := f.cfg.Wallet.EstimateFeeRate(f.ctx, urgentConfTarget)
	if err != nil {
		f.Infof("error estimating fee rate: %v", err)
		return f.HandleError(err)
	}

	// Send the instantout request to the server.
	instantOutResponse, err := f.cfg.InstantOutClient.RequestInstantLoopOut(
		f.ctx,
		&swapserverrpc.InstantLoopOutRequest{
			ReceiverKey:     keyRes.PubKey.SerializeCompressed(),
			SwapHash:        swapHash[:],
			Expiry:          initCtx.cltvExpiry,
			HtlcFeeRate:     uint64(feeRate),
			ReservationIds:  reservationIds,
			ProtocolVersion: CurrentRpcProtocolVersion(),
		},
	)
	if err != nil {
		return f.HandleError(err)
	}
	// Decode the invoice to check if the hash is valid.
	payReq, err := f.cfg.LndClient.DecodePaymentRequest(
		f.ctx, instantOutResponse.SwapInvoice,
	)
	if err != nil {
		return f.HandleError(err)
	}

	if swapHash != payReq.Hash {
		return f.HandleError(fmt.Errorf("invalid swap invoice hash: "+
			"expected %x got %x", preimage.Hash(), payReq.Hash))
	}
	serverPubkey, err := btcec.ParsePubKey(instantOutResponse.SenderKey)
	if err != nil {
		return f.HandleError(err)
	}

	// Create the address that we'll send the funds to.
	sweepAddress := initCtx.sweepAddress
	if sweepAddress == nil {
		sweepAddress, err = f.cfg.Wallet.NextAddr(
			f.ctx, lnwallet.DefaultAccountName,
			walletrpc.AddressType_TAPROOT_PUBKEY, false,
		)
		if err != nil {
			return f.HandleError(err)
		}
	}

	// Now we can create the instant out.
	instantOut := &InstantOut{
		SwapHash:         swapHash,
		swapPreimage:     preimage,
		protocolVersion:  ProtocolVersionFullReservation,
		initiationHeight: initCtx.initationHeight,
		outgoingChanSet:  initCtx.outgoingChanSet,
		CltvExpiry:       initCtx.cltvExpiry,
		clientPubkey:     keyRes.PubKey,
		serverPubkey:     serverPubkey,
		Value:            btcutil.Amount(reservationAmt),
		htlcFeeRate:      feeRate,
		swapInvoice:      instantOutResponse.SwapInvoice,
		Reservations:     reservations,
		keyLocator:       keyRes.KeyLocator,
		sweepAddress:     sweepAddress,
	}

	err = f.cfg.Store.CreateInstantLoopOut(f.ctx, instantOut)
	if err != nil {
		return f.HandleError(err)
	}

	f.InstantOut = instantOut

	return OnInit
}

// PollPaymentAcceptedAction locks the reservations, sends the payment to the
// server and polls the server for the payment status.
func (f *FSM) PollPaymentAcceptedAction(_ fsm.EventContext) fsm.EventType {
	// Now that we're doing the swap, we first lock the reservations
	// so that they can't be used for other swaps.
	for _, reservation := range f.InstantOut.Reservations {
		err := f.cfg.ReservationManager.LockReservation(
			f.ctx, reservation.ID,
		)
		if err != nil {
			return f.handleErrorAndUnlockReservations(err)
		}
	}

	// Now we send the payment to the server.
	payChan, paymentErrChan, err := f.cfg.RouterClient.SendPayment(
		f.ctx,
		lndclient.SendPaymentRequest{
			Invoice:  f.InstantOut.swapInvoice,
			Timeout:  defaultSendpaymentTimeout,
			MaxParts: defaultMaxParts,
			MaxFee:   getMaxRoutingFee(f.InstantOut.Value),
		},
	)
	if err != nil {
		f.Errorf("error sending payment: %v", err)
		return f.handleErrorAndUnlockReservations(err)
	}

	// We'll continuously poll the server for the payment status.
	pollPaymentTries := 0

	// We want to poll quickly the first time.
	timer := time.NewTimer(time.Second)
	for {
		select {
		case payRes := <-payChan:
			f.Debugf("payment result: %v", payRes)
			if payRes.State == lnrpc.Payment_FAILED {
				return f.handleErrorAndUnlockReservations(
					fmt.Errorf("payment failed: %v",
						payRes.FailureReason),
				)
			}
		case err := <-paymentErrChan:
			f.Errorf("error sending payment: %v", err)
			return f.handleErrorAndUnlockReservations(err)

		case <-f.ctx.Done():
			return f.handleErrorAndUnlockReservations(nil)

		case <-timer.C:
			res, err := f.cfg.InstantOutClient.PollPaymentAccepted(
				f.ctx,
				&swapserverrpc.PollPaymentAcceptedRequest{
					SwapHash: f.InstantOut.SwapHash[:],
				},
			)
			if err != nil {
				pollPaymentTries++
				if pollPaymentTries > 20 {
					return f.handleErrorAndUnlockReservations(err)
				}
			}
			if res != nil && res.Accepted {
				return OnPaymentAccepted
			}
			timer.Reset(defaultPollPaymentTime)
		}
	}
}

// BuildHTLCAction creates the htlc transaction, exchanges nonces with
// the server and sends the htlc signatures to the server.
func (f *FSM) BuildHTLCAction(eventCtx fsm.EventContext) fsm.EventType {
	htlcSessions, htlcClientNonces, err := f.InstantOut.createMusig2Session(
		f.ctx, f.cfg.Signer,
	)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	f.htlcMusig2Sessions = htlcSessions

	// Send the server the client nonces.
	htlcInitRes, err := f.cfg.InstantOutClient.InitHtlcSig(
		f.ctx,
		&swapserverrpc.InitHtlcSigRequest{
			SwapHash:         f.InstantOut.SwapHash[:],
			HtlcClientNonces: htlcClientNonces,
		},
	)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	if len(htlcInitRes.HtlcServerNonces) != len(f.InstantOut.Reservations) {
		return f.handleErrorAndUnlockReservations(
			errors.New("invalid number of server nonces"),
		)
	}

	htlcServerNonces, err := toNonces(htlcInitRes.HtlcServerNonces)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	// Now that our nonces are set, we can create and sign the htlc
	// transaction.
	htlcTx, err := f.InstantOut.createHtlcTransaction(f.cfg.Network)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	// Next we'll get our sweep tx signatures.
	htlcSigs, err := f.InstantOut.signMusig2Tx(
		f.ctx, f.cfg.Signer, htlcTx, f.htlcMusig2Sessions,
		htlcServerNonces,
	)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	// Send the server the htlc signatures.
	htlcRes, err := f.cfg.InstantOutClient.PushHtlcSig(
		f.ctx,
		&swapserverrpc.PushHtlcSigRequest{
			SwapHash:   f.InstantOut.SwapHash[:],
			ClientSigs: htlcSigs,
		},
	)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	// We can now finalize the htlc transaction.
	htlcTx, err = f.InstantOut.finalizeMusig2Transaction(
		f.ctx, f.cfg.Signer, f.htlcMusig2Sessions, htlcTx,
		htlcRes.ServerSigs,
	)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	f.InstantOut.finalizedHtlcTx = htlcTx

	return OnHtlcSigReceived
}

// PushPreimageAction pushes the preimage to the server. It also creates the
// sweepless sweep transaction and sends the signatures to the server. Finally,
// it publishes the sweepless sweep transaction. If any of the steps after
// pushing the preimage fail, the htlc timeout transaction will be published.
func (f *FSM) PushPreimageAction(eventCtx fsm.EventContext) fsm.EventType {
	// First we'll create the musig2 context.
	coopSessions, coopClientNonces, err := f.InstantOut.createMusig2Session(
		f.ctx, f.cfg.Signer,
	)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	f.sweeplessSweepSessions = coopSessions

	// Get the feerate for the coop sweep.
	feeRate, err := f.cfg.Wallet.EstimateFeeRate(f.ctx, normalConfTarget)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	pushPreImageRes, err := f.cfg.InstantOutClient.PushPreimage(
		f.ctx,
		&swapserverrpc.PushPreimageRequest{
			Preimage:        f.InstantOut.swapPreimage[:],
			ClientNonces:    coopClientNonces,
			ClientSweepAddr: f.InstantOut.sweepAddress.String(),
			MusigTxFeeRate:  uint64(feeRate),
		},
	)
	// Now that we have revealed the preimage, if any following step fail,
	// we'll need to publish the htlc tx.
	if err != nil {
		f.LastActionError = err
		return OnErrorPublishHtlc
	}

	// Now that we have the sweepless sweep signatures we can build and
	// publish the sweepless sweep transaction.
	sweepTx, err := f.InstantOut.createSweeplessSweepTx(feeRate)
	if err != nil {
		f.LastActionError = err
		return OnErrorPublishHtlc
	}

	coopServerNonces, err := toNonces(pushPreImageRes.ServerNonces)
	if err != nil {
		f.LastActionError = err
		return OnErrorPublishHtlc
	}

	// Next we'll get our sweep tx signatures.
	_, err = f.InstantOut.signMusig2Tx(
		f.ctx, f.cfg.Signer, sweepTx, f.sweeplessSweepSessions,
		coopServerNonces,
	)
	if err != nil {
		f.LastActionError = err
		return OnErrorPublishHtlc
	}

	// Now we'll finalize the sweepless sweep transaction.
	sweepTx, err = f.InstantOut.finalizeMusig2Transaction(
		f.ctx, f.cfg.Signer, f.sweeplessSweepSessions, sweepTx,
		pushPreImageRes.Musig2SweepSigs,
	)
	if err != nil {
		f.LastActionError = err
		return OnErrorPublishHtlc
	}

	f.InstantOut.FinalizedSweeplessSweepTx = sweepTx
	txHash := f.InstantOut.FinalizedSweeplessSweepTx.TxHash()

	f.InstantOut.SweepTxHash = &txHash

	return OnSweeplessSweepBuilt
}

// PublishSweeplessSweepAction publishes the sweepless sweep transaction.
func (f *FSM) PublishSweeplessSweepAction(eventCtx fsm.EventContext) fsm.EventType {
	if f.InstantOut.FinalizedSweeplessSweepTx == nil {
		return f.HandleError(errors.New("sweep tx not finalized"))
	}

	txLabel := fmt.Sprintf("sweepless-sweep-%v",
		f.InstantOut.swapPreimage.Hash())

	sweepTx := f.InstantOut.FinalizedSweeplessSweepTx

	// Publish the sweepless sweep transaction.
	err := f.cfg.Wallet.PublishTransaction(f.ctx, sweepTx, txLabel)
	if err != nil {
		return f.HandleError(err)
	}

	return OnSweeplessSweepPublished
}

// WaitForSweeplessSweepConfirmedAction waits for the sweepless sweep
// transaction to be confirmed.
func (f *FSM) WaitForSweeplessSweepConfirmedAction(
	eventCtx fsm.EventContext) fsm.EventType {

	pkscript, err := txscript.PayToAddrScript(f.InstantOut.sweepAddress)
	if err != nil {
		return f.HandleError(err)
	}

	confChan, confErrChan, err := f.cfg.ChainNotifier.
		RegisterConfirmationsNtfn(
			f.ctx, f.InstantOut.SweepTxHash, pkscript,
			1, f.InstantOut.initiationHeight,
		)
	if err != nil {
		return f.HandleError(err)
	}

	for {
		select {
		case spendErr := <-confErrChan:
			f.LastActionError = spendErr
			f.Errorf("error listening for sweepless sweep "+
				"confirmation: %v", spendErr)

			return OnErrorPublishHtlc

		case conf := <-confChan:
			f.InstantOut.
				sweepConfirmationHeight = conf.BlockHeight

			return OnSweeplessSweepConfirmed
		}
	}
}

// PublishHtlcAction publishes the htlc transaction and the htlc sweep
// transaction.
func (f *FSM) PublishHtlcAction(eventCtx fsm.EventContext) fsm.EventType {
	// Publish the htlc transaction.
	err := f.cfg.Wallet.PublishTransaction(
		f.ctx, f.InstantOut.finalizedHtlcTx,
		fmt.Sprintf("htlc-%v", f.InstantOut.swapPreimage.Hash()),
	)
	if err != nil {
		return f.HandleError(err)
	}

	txHash := f.InstantOut.finalizedHtlcTx.TxHash()
	f.Debugf("published htlc tx: %v", txHash)

	// We'll now wait for the htlc to be confirmed.
	confChan, confErrChan, err := f.cfg.ChainNotifier.
		RegisterConfirmationsNtfn(
			f.ctx, &txHash,
			f.InstantOut.finalizedHtlcTx.TxOut[0].PkScript,
			1, f.InstantOut.initiationHeight,
		)
	if err != nil {
		return f.HandleError(err)
	}
	for {
		select {
		case spendErr := <-confErrChan:
			return f.HandleError(spendErr)

		case <-confChan:
			return OnHtlcPublished
		}
	}
}

// PublishHtlcSweepAction publishes the htlc sweep transaction.
func (f *FSM) PublishHtlcSweepAction(eventCtx fsm.EventContext) fsm.EventType {
	// Create a feerate that will confirm the htlc quickly.
	feeRate, err := f.cfg.Wallet.EstimateFeeRate(f.ctx, urgentConfTarget)
	if err != nil {
		return f.HandleError(err)
	}

	getInfo, err := f.cfg.LndClient.GetInfo(f.ctx)
	if err != nil {
		return f.HandleError(err)
	}

	// We can immediately publish the htlc sweep transaction.
	htlcSweepTx, err := f.InstantOut.generateHtlcSweepTx(
		f.ctx, f.cfg.Signer, feeRate, f.cfg.Network, getInfo.BlockHeight,
	)
	if err != nil {
		return f.HandleError(err)
	}

	label := fmt.Sprintf("htlc-sweep-%v", f.InstantOut.swapPreimage.Hash())

	err = f.cfg.Wallet.PublishTransaction(f.ctx, htlcSweepTx, label)
	if err != nil {
		log.Errorf("error publishing htlc sweep tx: %v", err)
		return f.HandleError(err)
	}

	sweepTxHash := htlcSweepTx.TxHash()

	f.InstantOut.SweepTxHash = &sweepTxHash

	return OnHtlcSweepPublished
}

// WaitForHtlcSweepConfirmedAction waits for the htlc sweep transaction to be
// confirmed.
func (f *FSM) WaitForHtlcSweepConfirmedAction(
	eventCtx fsm.EventContext) fsm.EventType {

	sweepPkScript, err := txscript.PayToAddrScript(
		f.InstantOut.sweepAddress,
	)
	if err != nil {
		return f.HandleError(err)
	}

	confChan, confErrChan, err := f.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		f.ctx, f.InstantOut.SweepTxHash, sweepPkScript,
		1, f.InstantOut.initiationHeight,
	)
	if err != nil {
		return f.HandleError(err)
	}

	f.Debugf("waiting for htlc sweep tx %v to be confirmed",
		f.InstantOut.SweepTxHash)

	for {
		select {
		case spendErr := <-confErrChan:
			return f.HandleError(spendErr)

		case conf := <-confChan:
			f.InstantOut.
				sweepConfirmationHeight = conf.BlockHeight

			return OnHtlcSwept
		}
	}
}

// handleErrorAndUnlockReservations handles an error and unlocks the
// reservations.
func (f *FSM) handleErrorAndUnlockReservations(err error) fsm.EventType {
	// We might get here from a canceled context, we create a new context
	// with a timeout to unlock the reservations.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	// Unlock the reservations.
	for _, reservation := range f.InstantOut.Reservations {
		err := f.cfg.ReservationManager.UnlockReservation(
			ctx, reservation.ID,
		)
		if err != nil {
			f.Errorf("error unlocking reservation: %v", err)
			return f.HandleError(err)
		}
	}

	// We're also sending the server a cancel message so that it can
	// release the reservations. This can be done in a goroutine as we
	// wan't to fail the fsm early.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		_, cancelErr := f.cfg.InstantOutClient.CancelInstantSwap(
			ctx, &swapserverrpc.CancelInstantSwapRequest{
				SwapHash: f.InstantOut.SwapHash[:],
			},
		)
		if cancelErr != nil {
			// We'll log the error but not return it as we want to return the
			// original error.
			f.Debugf("error sending cancel message: %v", cancelErr)
		}
	}()

	return f.HandleError(err)
}

func getMaxRoutingFee(amt btcutil.Amount) btcutil.Amount {
	return swap.CalcFee(amt, maxRoutingFeeBase, maxRoutingFeeRate)
}
