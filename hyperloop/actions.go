package hyperloop

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	looprpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (

	// Define route independent max routing fees. We have currently no way
	// to get a reliable estimate of the routing fees. Best we can do is
	// the minimum routing fees, which is not very indicative.
	maxRoutingFeeBase = btcutil.Amount(10)

	maxRoutingFeeRate = int64(20000)

	// defaultSendpaymentTimeout is the default timeout for the swap invoice.
	defaultSendpaymentTimeout = time.Minute * 5
)

// initHyperloopContext contains the request parameters for a hyperloop.
type initHyperloopContext struct {
	hyperloopID     ID
	swapAmt         btcutil.Amount
	outgoingChanSet *loopdb.ChannelSet
	publishTime     time.Time
	sweepAddr       btcutil.Address
}

// initHyperloopAction is the action that initializes the hyperloop.
func (f *FSM) initHyperloopAction(eventCtx fsm.EventContext) fsm.EventType {
	initCtx, ok := eventCtx.(*initHyperloopContext)
	if !ok {
		return f.HandleError(fsm.ErrInvalidContextType)
	}

	// Create a new receiver key descriptor.
	receiverKeyDesc, err := f.cfg.Wallet.DeriveNextKey(f.ctx, 42069)
	if err != nil {
		return f.HandleError(err)
	}

	// Create a new swap preimage.
	var swapPreimage lntypes.Preimage
	if _, err := rand.Read(swapPreimage[:]); err != nil {
		return f.HandleError(err)
	}

	hyperloop := newHyperLoop(
		initCtx.hyperloopID, initCtx.publishTime, initCtx.swapAmt,
		receiverKeyDesc, swapPreimage, initCtx.sweepAddr,
	)

	// Request the hyperloop from the server.
	registerRes, err := f.cfg.HyperloopClient.RegisterHyperloop(
		f.ctx, &looprpc.RegisterHyperloopRequest{
			ReceiverKey: hyperloop.
				KeyDesc.PubKey.SerializeCompressed(),
			SwapHash:                    hyperloop.SwapHash[:],
			Amt:                         int64(hyperloop.Amt),
			PrivateHyperloopId:          hyperloop.ID[:],
			PrivateHyperloopPublishTime: hyperloop.PublishTime.Unix(),
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	senderKey, err := btcec.ParsePubKey(registerRes.ServerKey)
	if err != nil {
		return f.HandleError(err)
	}

	hyperloop.addInitialHyperLoopInfo(
		chainfee.SatPerKWeight(registerRes.HyperloopFee),
		uint32(registerRes.HyperloopExpiry),
		registerRes.HtlcExpiry,
		senderKey,
		registerRes.Invoice,
	)

	f.hyperloop = hyperloop

	// Now that we have the hyperloop response from the server, we can
	// create the hyperloop in the database.
	err = f.cfg.Store.CreateHyperloop(f.ctx, hyperloop)
	if err != nil {
		return f.HandleError(err)
	}

	return OnInit
}

// registerHyperloopAction is the action that registers the hyperloop by
// paying the server.
func (f *FSM) registerHyperloopAction(eventCtx fsm.EventContext) fsm.EventType {
	// Create a context which we can cancel if we're registered.
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()

	// Poll the server to check if we're registered.
	go func() {
		checkFunc := func(res *looprpc.HyperloopNotificationStreamResponse) (
			bool, error) {
			for _, participant := range res.Participants {
				if bytes.Equal(participant.SwapHash,
					f.hyperloop.SwapHash[:]) {
					return true, nil
				}
			}

			if res.HyperloopTxid != "" {
				return false, errors.New("registration failed")
			}

			return false, nil
		}
		_, err := f.waitForState(ctx, checkFunc)
		if err != nil {
			f.handleAsyncError(err)
			return
		}

		err = f.SendEvent(OnRegistered, nil)
		if err != nil {
			f.handleAsyncError(err)
			return
		}
	}()

	// Dispatch the swap payment.
	paymentChan, errChan, err := f.cfg.Router.SendPayment(
		f.ctx, lndclient.SendPaymentRequest{
			Invoice: f.hyperloop.SwapInvoice,
			MaxFee:  getMaxRoutingFee(f.hyperloop.Amt),
			Timeout: defaultSendpaymentTimeout,
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	go func() {
		select {
		case <-f.ctx.Done():
			return
		case err := <-errChan:
			f.handleAsyncError(err)
		case res := <-paymentChan:
			if res.State == lnrpc.Payment_FAILED {
				err = f.SendEvent(OnPaymentFailed, nil)
				if err != nil {
					f.handleAsyncError(err)
					return
				}
			}
		}
	}()

	return fsm.NoOp
}

// waitForPublishAction is the action that waits for the hyperloop to be
// published.
func (f *FSM) waitForPublishAction(_ fsm.EventContext) fsm.EventType {
	go func() {
		checkFunc := func(res *looprpc.HyperloopNotificationStreamResponse) (
			bool, error) {
			if res.HyperloopTxid != "" {
				return true, nil
			}

			return false, nil
		}
		res, err := f.waitForState(f.ctx, checkFunc)
		if err != nil {
			f.handleAsyncError(err)
		}

		// If we're published, the list of participants is final.
		participants, err := rpcParticipantToHyperloopParticipant(
			res.Participants, f.cfg.ChainParams,
		)
		if err != nil {
			f.handleAsyncError(err)
			return
		}
		f.hyperloop.registerParticipants(participants)

		err = f.SendEvent(OnPublished, nil)
		if err != nil {
			f.handleAsyncError(err)
			return
		}
	}()

	return fsm.NoOp
}

// waitForConfirmationAction is the action that waits for the hyperloop to be
// confirmed.
func (f *FSM) waitForConfirmationAction(_ fsm.EventContext) fsm.EventType {
	pkScript, err := f.hyperloop.getHyperLoopScript()
	if err != nil {
		return f.HandleError(err)
	}

	subChan, errChan, err := f.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		f.ctx, nil, pkScript, 2, 100, //todo height hint
	)
	if err != nil {
		return f.HandleError(err)
	}

	go func() {
		for {
			select {
			case <-f.ctx.Done():
				return
			case err := <-errChan:
				log.Errorf("Error waiting for confirmation: %v", err)
				f.handleAsyncError(err)
				return
			case conf := <-subChan:
				f.hyperloop.ConfirmationHeight = int32(conf.BlockHeight)
				f.hyperloop.ConfirmedOutpoint, err = getOutpointFromTx(
					conf.Tx, pkScript,
				)
				if err != nil {
					f.handleAsyncError(err)
				}
				err := f.SendEvent(OnConfirmed, nil)
				if err != nil {
					f.handleAsyncError(err)
					return
				}
			}
		}
	}()

	return fsm.NoOp
}

// pushHtlcNonceAction is the action that pushes the htlc nonce to the server.
func (f *FSM) pushHtlcNonceAction(_ fsm.EventContext) fsm.EventType {
	err := f.hyperloop.startHtlcSession(f.ctx, f.cfg.Signer)
	if err != nil {
		return f.HandleError(err)
	}

	res, err := f.cfg.HyperloopClient.PushHyperloopHtlcNonce(
		f.ctx, &looprpc.PushHyperloopHtlcNonceRequest{
			SwapHash:    f.hyperloop.SwapHash[:],
			HyperloopId: f.hyperloop.ID[:],
			HtlcNonce:   f.hyperloop.HtlcMusig2Session.PublicNonce[:],
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	copy(f.hyperloop.SweepServerNonce[:], res.ServerHtlcNonce)
	f.hyperloop.HtlcFeeRate = chainfee.SatPerKWeight(res.HtlcFeeRate)

	return OnPushedHtlcNonce
}

// waitForReadyForHtlcSignAction is the action that waits for the server to be
// ready for the htlc sign.
func (f *FSM) waitForReadyForHtlcSignAction(_ fsm.EventContext) fsm.EventType {
	go func() {
		checkFunc := func(res *looprpc.HyperloopNotificationStreamResponse) (
			bool, error) {
			// If the length of htlc nonces is equal to the
			// number of participants, we're ready to sign.
			return len(res.Participants) == len(res.HtlcNonces), nil
		}
		res, err := f.waitForState(f.ctx, checkFunc)
		if err != nil {
			f.handleAsyncError(err)
			return
		}

		var nonces [][66]byte
		for _, htlcNonce := range res.HtlcNonces {
			htlcNonce := htlcNonce

			var nonce [66]byte
			copy(nonce[:], htlcNonce)
			nonces = append(nonces, nonce)
		}

		err = f.hyperloop.registerHtlcNonces(f.ctx, f.cfg.Signer, nonces)
		if err != nil {
			f.handleAsyncError(err)
			return
		}

		err = f.SendEvent(OnReadyForHtlcSig, nil)
		if err != nil {
			f.handleAsyncError(err)
			return
		}

	}()

	return fsm.NoOp
}

// pushHtlcSigAction is the action that pushes the htlc signatures to the server.
func (f *FSM) pushHtlcSigAction(_ fsm.EventContext) fsm.EventType {
	htlcTx, err := f.hyperloop.getHyperLoopHtlcTx(f.cfg.ChainParams)
	if err != nil {
		return f.HandleError(err)
	}

	sig, err := f.hyperloop.getSigForTx(
		f.ctx, f.cfg.Signer, htlcTx, f.hyperloop.HtlcMusig2Session.SessionID,
	)
	if err != nil {
		return f.HandleError(err)
	}

	_, err = f.cfg.HyperloopClient.PushHyperloopHtlcSig(
		f.ctx, &looprpc.PushHyperloopHtlcSigRequest{
			SwapHash:    f.hyperloop.SwapHash[:],
			HyperloopId: f.hyperloop.ID[:],
			HtlcSig:     sig,
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	return OnPushedHtlcSig
}

// waitForHtlcSig is the action where we poll the server for the htlc signature.
func (f *FSM) waitForHtlcSig(_ fsm.EventContext) fsm.EventType {
	go func() {
		checkFunc := func(res *looprpc.HyperloopNotificationStreamResponse) (
			bool, error) {
			return res.FinalHtlcSig != nil, nil
		}
		res, err := f.waitForState(f.ctx, checkFunc)
		if err != nil {
			f.handleAsyncError(err)
			return
		}

		// Try to register the htlc sig, this will verify the sig is valid.
		err = f.hyperloop.registerHtlcSig(f.cfg.ChainParams, res.FinalHtlcSig)
		if err != nil {
			f.handleAsyncError(fmt.Errorf("invalid htlc sig: %v", err))
		}
		err = f.SendEvent(OnReceivedHtlcSig, nil)
		if err != nil {
			f.handleAsyncError(err)
			return
		}
	}()

	return fsm.NoOp
}

// pushPreimageAction is the action that pushes the preimage to the server.
func (f *FSM) pushPreimageAction(_ fsm.EventContext) fsm.EventType {
	// Start the sweep session.
	err := f.hyperloop.startSweeplessSession(f.ctx, f.cfg.Signer)
	if err != nil {
		return f.HandleError(err)
	}

	res, err := f.cfg.HyperloopClient.PushHyperloopPreimage(
		f.ctx, &looprpc.PushHyperloopPreimageRequest{
			HyperloopId: f.hyperloop.ID[:],
			Preimage:    f.hyperloop.SwapPreimage[:],
			SweepNonce:  f.hyperloop.SweeplessSweepMusig2Session.PublicNonce[:],
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	copy(f.hyperloop.SweepServerNonce[:], res.ServerSweepNonce)
	f.hyperloop.SweeplessSweepFeeRate = chainfee.SatPerKWeight(res.SweepFeeRate)

	return OnPushedPreimage
}

// waitForReadyForSweepAction is the action that waits for the server to be
// ready for the sweep.
func (f *FSM) waitForReadyForSweepAction(_ fsm.EventContext) fsm.EventType {
	go func() {
		checkFunc := func(res *looprpc.HyperloopNotificationStreamResponse) (
			bool, error) {
			return len(res.Participants) == len(res.SweeplessSweepNonces), nil
		}
		res, err := f.waitForState(f.ctx, checkFunc)
		if err != nil {
			f.handleAsyncError(err)
			return
		}

		var nonces [][66]byte
		for _, sweepNonce := range res.SweeplessSweepNonces {
			sweepNonce := sweepNonce

			var nonce [66]byte
			copy(nonce[:], sweepNonce)
			nonces = append(nonces, nonce)
		}

		err = f.hyperloop.registerSweeplessSweepNonces(f.ctx, f.cfg.Signer, nonces)
		if err != nil {
			f.handleAsyncError(err)
			return
		}

		err = f.SendEvent(OnReadyForSweeplessSweepSig, nil)
		if err != nil {
			f.handleAsyncError(err)
			return
		}

	}()

	return fsm.NoOp
}

func (f *FSM) pushSweepSigAction(_ fsm.EventContext) fsm.EventType {
	sweepTx, err := f.hyperloop.getHyperLoopSweeplessSweepTx()
	if err != nil {
		return f.HandleError(err)
	}

	sig, err := f.hyperloop.getSigForTx(
		f.ctx, f.cfg.Signer, sweepTx,
		f.hyperloop.SweeplessSweepMusig2Session.SessionID,
	)
	if err != nil {
		return f.HandleError(err)
	}

	_, err = f.cfg.HyperloopClient.PushHyperloopSweeplessSweepSig(
		f.ctx, &looprpc.PushHyperloopSweeplessSweepSigRequest{
			HyperloopId: f.hyperloop.ID[:],
			SwapHash:    f.hyperloop.SwapHash[:],
			SweepSig:    sig,
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	return OnPushedSweeplessSweepSig
}

// waitForSweeplessSweepConfirmationAction is the action that waits for the
// sweepless sweep to be confirmed.
func (f *FSM) waitForSweeplessSweepConfirmationAction(_ fsm.EventContext,
) fsm.EventType {

	sweeplessSweepTx, err := f.hyperloop.getHyperLoopSweeplessSweepTx()
	if err != nil {
		return f.HandleError(err)
	}

	sweeplessSweepTxHash := sweeplessSweepTx.TxHash()

	confChan, errChan, err := f.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		f.ctx, &sweeplessSweepTxHash, nil, 2, f.hyperloop.ConfirmationHeight,
	)
	if err != nil {
		return f.HandleError(err)
	}

	go func() {
		for {
			select {
			case <-f.ctx.Done():
				return
			case err := <-errChan:
				log.Errorf("Error waiting for confirmation: %v", err)
				f.handleAsyncError(err)
				return
			case <-confChan:
				err := f.SendEvent(OnSweeplessSweepConfirmed, nil)
				if err != nil {
					f.handleAsyncError(err)
					return
				}
			}
		}
	}()

	return fsm.NoOp
}

// waitForState polls the server for the hyperloop status until the
// status matches the expected status. Once the status matches, it returns the
// response.
func (f *FSM) waitForState(ctx context.Context,
	checkFunc func(*looprpc.HyperloopNotificationStreamResponse) (bool, error),
) (*looprpc.HyperloopNotificationStreamResponse, error) {

	// Create a context which we can cancel if we're published.
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return nil, errors.New("context canceled")
		case <-time.After(time.Second * 5):
			if f.lastNotification == nil {
				continue
			}

			status, err := checkFunc(f.lastNotification)
			if err != nil {
				return nil, err
			}
			if status {
				return f.lastNotification, nil
			}
		}
	}
}

// handleAsyncError is a helper method that logs an error and sends an error
// event to the FSM
func (f *FSM) handleAsyncError(err error) {
	f.LastActionError = err
	f.Errorf("Error on async action: %v", err)
	err2 := f.SendEvent(fsm.OnError, err)
	if err2 != nil {
		f.Errorf("Error sending event: %v", err2)
	}
}

// getMaxRoutingFee returns the maximum routing fee for a given amount.
func getMaxRoutingFee(amt btcutil.Amount) btcutil.Amount {
	return swap.CalcFee(amt, maxRoutingFeeBase, maxRoutingFeeRate)
}

// getOutpointFromTx returns the outpoint of the pkScript in the tx.
func getOutpointFromTx(tx *wire.MsgTx, pkScript []byte) (*wire.OutPoint,
	error) {

	for i, txOut := range tx.TxOut {
		if bytes.Equal(pkScript, txOut.PkScript) {
			txHash := tx.TxHash()
			return wire.NewOutPoint(&txHash, uint32(i)), nil
		}
	}
	return nil, errors.New("pk script not found in tx")
}

// rpcParticipantToHyperloopParticipant converts a slice of rpc participants to
// to a slice of hyperloop participants.
func rpcParticipantToHyperloopParticipant(rpcParticipant []*looprpc.
	HyperLoopParticipant, params *chaincfg.Params) ([]*HyperLoopParticipant, error) {

	participants := make([]*HyperLoopParticipant, 0, len(rpcParticipant))
	for _, p := range rpcParticipant {
		p := p
		pubkey, err := btcec.ParsePubKey(p.ParticipantKey)
		if err != nil {
			return nil, err
		}

		swapHash, err := lntypes.MakeHash(p.SwapHash)
		if err != nil {
			return nil, err
		}

		sweepAddr, err := btcutil.DecodeAddress(p.SweepAddr, params)
		if err != nil {
			return nil, err
		}

		participants = append(participants, &HyperLoopParticipant{
			SwapHash:     swapHash,
			Amt:          btcutil.Amount(p.Amt),
			Pubkey:       pubkey,
			SweepAddress: sweepAddr,
		})
	}

	return participants, nil
}
