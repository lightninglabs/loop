package instantout

import (
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
	loop_rpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	ErrInsufficientReservationBalance = errors.New(
		"insufficient balance in reservations",
	)

	ErrReservationsMustBeSwapAmount = errors.New(
		"reservations total amount must be equal swap amount",
	)

	UrgentConfTarget = int32(3)
	NormalConfTarget = int32(6)
)

// InitInstantOutCtx contains the context for the InitInstantOutAction.
type InitInstantOutCtx struct {
	cltvExpiry      int32
	reservations    []reservation.ID
	initationHeight int32
	outgoingChanSet loopdb.ChannelSet
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
		res, err := f.cfg.ReservationManager.GetReservation(
			f.ctx, reservationId,
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
		reservationIds = append(reservationIds, reservationId[:])
		reservations = append(reservations, res)
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
	feeRate, err := f.cfg.Wallet.EstimateFeeRate(f.ctx, UrgentConfTarget)
	if err != nil {
		f.Infof("error estimating fee rate: %v", err)
		return f.HandleError(err)
	}

	// Send the instantout request to the server.
	instantOutResponse, err := f.cfg.InstantOutClient.RequestInstantLoopOut(
		f.ctx,
		&loop_rpc.InstantLoopOutRequest{
			Amt:            reservationAmt,
			ReceiverKey:    keyRes.PubKey.SerializeCompressed(),
			SwapHash:       swapHash[:],
			Expiry:         initCtx.cltvExpiry,
			HtlcFeeRate:    uint64(feeRate),
			ReservationIds: reservationIds,
			ProtocolVersion: loop_rpc.
				InstantOutProtocolVersion_INSTANTOUT_FULL_RESERVATION,
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

	if preimage.Hash() != payReq.Hash {
		return f.HandleError(fmt.Errorf("invalid swap invoice hash: "+
			"expected %x got %x", preimage.Hash(), payReq.Hash))
	}
	serverPubkey, err := btcec.ParsePubKey(instantOutResponse.SenderKey)
	if err != nil {
		return f.HandleError(err)
	}

	// Create the address that we'll send the funds to
	sweepAddress, err := f.cfg.Wallet.NextAddr(
		f.ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return f.HandleError(err)
	}

	// Now we can create the instant out.
	instantOut := &InstantOut{
		SwapHash:         swapHash,
		SwapPreimage:     preimage,
		ProtocolVersion:  ProtocolVersionFullReservation,
		InitiationHeight: initCtx.initationHeight,
		OutgoingChanSet:  initCtx.outgoingChanSet,
		CltvExpiry:       initCtx.cltvExpiry,
		ClientPubkey:     keyRes.PubKey,
		ServerPubkey:     serverPubkey,
		Value:            btcutil.Amount(reservationAmt),
		HtlcFeeRate:      feeRate,
		SwapInvoice:      instantOutResponse.SwapInvoice,
		Reservations:     reservations,
		KeyLocator:       keyRes.KeyLocator,
		SweepAddress:     sweepAddress,
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
func (f *FSM) PollPaymentAcceptedAction(
	eventCtx fsm.EventContext) fsm.EventType {

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
			Invoice: f.InstantOut.SwapInvoice,
			Timeout: time.Minute * 5,
		},
	)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	// We'll continuously poll the server for the payment status.
	for {
		select {
		case payRes := <-payChan:
			f.Debugf("payment result: %v", payRes)
			if payRes.FailureReason !=
				lnrpc.PaymentFailureReason_FAILURE_REASON_NONE {

				return f.handleErrorAndUnlockReservations(
					fmt.Errorf("payment failed: %v",
						payRes.FailureReason),
				)
			}

		case err := <-paymentErrChan:
			return f.handleErrorAndUnlockReservations(err)

		case <-f.ctx.Done():
			return fsm.NoOp

		default:
			res, err := f.cfg.InstantOutClient.PollPaymentAccepted(
				f.ctx, &loop_rpc.PollPaymentAcceptedRequest{
					SwapHash: f.InstantOut.SwapHash[:],
				},
			)
			if err != nil {
				return f.handleErrorAndUnlockReservations(err)
			}
			if res.Accepted {
				return OnPaymentAccepted
			}

			time.Sleep(time.Second)
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
		&loop_rpc.InitHtlcSigRequest{
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
		&loop_rpc.PushHtlcSigRequest{
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

	f.InstantOut.FinalizedHtlcTx = htlcTx

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
	feeRate, err := f.cfg.Wallet.EstimateFeeRate(f.ctx, NormalConfTarget)
	if err != nil {
		return f.handleErrorAndUnlockReservations(err)
	}

	pushPreImageRes, err := f.cfg.InstantOutClient.PushPreimage(
		f.ctx,
		&loop_rpc.PushPreimageRequest{
			Preimage:        f.InstantOut.SwapPreimage[:],
			ClientNonces:    coopClientNonces,
			ClientSweepAddr: f.InstantOut.SweepAddress.String(),
			MusigTxFeeRate:  uint64(feeRate),
		},
	)
	// Now that we have revealed the preimage, if any following step fail,
	// we'll need to publish the htlc timeout tx.
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

	txLabel := fmt.Sprintf("sweepless-sweep-%v",
		f.InstantOut.SwapPreimage.Hash())

	// Publish the sweepless sweep transaction.
	err = f.cfg.Wallet.PublishTransaction(f.ctx, sweepTx, txLabel)
	if err != nil {
		f.LastActionError = err
		return OnErrorPublishHtlc
	}

	f.InstantOut.FinalizedSweeplessSweepTx = sweepTx
	txHash := f.InstantOut.FinalizedSweeplessSweepTx.TxHash()

	f.InstantOut.SweepTxHash = &txHash

	return OnSweeplessSweepPublished
}

// WaitForSweeplessSweepConfirmedAction waits for the sweepless sweep
// transaction to be confirmed.
func (f *FSM) WaitForSweeplessSweepConfirmedAction(
	eventCtx fsm.EventContext) fsm.EventType {

	pkscript, err := txscript.PayToAddrScript(f.InstantOut.SweepAddress)
	if err != nil {
		return f.HandleError(err)
	}

	confChan, confErrChan, err := f.cfg.ChainNotifier.
		RegisterConfirmationsNtfn(
			f.ctx, f.InstantOut.SweepTxHash, pkscript,
			1, f.InstantOut.InitiationHeight,
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
				SweepConfirmationHeight = conf.BlockHeight

			return OnSweeplessSweepConfirmed
		}
	}
}

// PublishHtlcAction publishes the htlc transaction and the htlc sweep
// transaction.
func (f *FSM) PublishHtlcAction(eventCtx fsm.EventContext) fsm.EventType {
	// Publish the htlc transaction.
	err := f.cfg.Wallet.PublishTransaction(
		f.ctx, f.InstantOut.FinalizedHtlcTx,
		fmt.Sprintf("htlc-%v", f.InstantOut.SwapPreimage.Hash()),
	)
	if err != nil {
		return f.HandleError(err)
	}

	// Create a feerate that will confirm the htlc quickly.
	feeRate, err := f.cfg.Wallet.EstimateFeeRate(f.ctx, UrgentConfTarget)
	if err != nil {
		return f.HandleError(err)
	}

	// We can immediately publish the htlc sweep transaction
	htlcSweepTx, err := f.InstantOut.generateHtlcSweepTx(
		f.ctx, f.cfg.Signer, feeRate, f.cfg.Network,
	)
	if err != nil {
		return f.HandleError(err)
	}

	txLabel := fmt.Sprintf("htlc-sweep-%v",
		f.InstantOut.SwapPreimage.Hash())

	err = f.cfg.Wallet.PublishTransaction(f.ctx, htlcSweepTx, txLabel)
	if err != nil {
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

	htlc, err := f.InstantOut.getHtlc(f.cfg.Network)
	if err != nil {
		return f.HandleError(err)
	}

	confChan, confErrChan, err := f.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		f.ctx, f.InstantOut.SweepTxHash, htlc.PkScript,
		1, f.InstantOut.InitiationHeight,
	)
	if err != nil {
		return f.HandleError(err)
	}

	for {
		select {
		case spendErr := <-confErrChan:
			f.LastActionError = spendErr
			return OnErrorPublishHtlc

		case conf := <-confChan:
			f.InstantOut.
				SweepConfirmationHeight = conf.BlockHeight

			return OnHtlcSwept
		}
	}
}

// handleErrorAndUnlockReservations handles an error and unlocks the
// reservations.
func (f *FSM) handleErrorAndUnlockReservations(err error) fsm.EventType {
	// Unlock the reservations.
	for _, reservation := range f.InstantOut.Reservations {
		err := f.cfg.ReservationManager.UnlockReservation(
			f.ctx, reservation.ID,
		)
		if err != nil {
			return f.HandleError(err)
		}
	}

	return f.HandleError(err)
}
