package hyperloop

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
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
	swapAmt          btcutil.Amount
	private          bool
	publishTime      time.Time
	sweepAddr        btcutil.Address
	initiationHeight int32
}

// initHyperloopAction is the action that initializes the hyperloop.
func (f *FSM) initHyperloopAction(ctx context.Context,
	eventCtx fsm.EventContext) fsm.EventType {

	f.ValLock.Lock()
	defer f.ValLock.Unlock()

	initCtx, ok := eventCtx.(*initHyperloopContext)
	if !ok {
		return f.HandleError(fsm.ErrInvalidContextType)
	}

	// Create a new receiver key descriptor.
	receiverKeyDesc, err := f.cfg.Wallet.DeriveNextKey(ctx, 42069)
	if err != nil {
		return f.HandleError(err)
	}

	// Create a new swap preimage.
	var swapPreimage lntypes.Preimage
	if _, err := rand.Read(swapPreimage[:]); err != nil {
		return f.HandleError(err)
	}

	hyperloop := newHyperLoop(
		initCtx.publishTime, initCtx.swapAmt, receiverKeyDesc,
		swapPreimage, initCtx.sweepAddr, initCtx.initiationHeight,
	)

	// Request the hyperloop from the server.
	registerRes, err := f.cfg.HyperloopClient.RegisterHyperloop(
		ctx, &swapserverrpc.RegisterHyperloopRequest{
			ReceiverKey: hyperloop.
				KeyDesc.PubKey.SerializeCompressed(),
			SwapHash:                 hyperloop.SwapHash[:],
			Amt:                      int64(hyperloop.Amt),
			Private:                  initCtx.private,
			HyperloopPublishTimeUnix: hyperloop.PublishTime.Unix(),
			SweepAddr:                hyperloop.SweepAddr.String(),
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	f.Debugf("Registered hyperloop: %x", registerRes.HyperloopId)

	var hyperloopId ID
	copy(hyperloopId[:], registerRes.HyperloopId)

	senderKey, err := btcec.ParsePubKey(registerRes.ServerKey)
	if err != nil {
		return f.HandleError(err)
	}

	hyperloop.addInitialHyperLoopInfo(
		hyperloopId,
		uint32(registerRes.HyperloopCsvExpiry),
		registerRes.HtlcCltvExpiry,
		senderKey,
		registerRes.Invoice,
	)

	f.hyperloop = hyperloop
	f.Val = hyperloop

	// TODO: implement sql store.
	// Now that we have the hyperloop response from the server, we can
	// create the hyperloop in the database.
	// err = f.cfg.Store.CreateHyperloop(ctx, hyperloop)
	// if err != nil {
	// 	return f.HandleError(err)
	// }

	return OnInit
}

// registerHyperloopAction is the action that registers the hyperloop by
// paying the server.
func (f *FSM) registerHyperloopAction(ctx context.Context,
	eventCtx fsm.EventContext) fsm.EventType {

	// Create a context which we can cancel if we're registered.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Poll the server to check if we're registered.
	checkFunc := func(res *swapserverrpc.
		HyperloopNotificationStreamResponse) (bool, error) {

		p, err := f.cfg.HyperloopClient.FetchHyperloopParticipants(
			ctx,
			&swapserverrpc.FetchHyperloopParticipantsRequest{
				HyperloopId: f.hyperloop.ID[:],
			},
		)
		if err != nil {
			return false, err
		}

		for _, participant := range p.Participants {
			if bytes.Equal(participant.PublicKey,
				f.hyperloop.KeyDesc.
					PubKey.SerializeCompressed()) {

				return true, nil
			}
		}

		// If the server is already further than the pending state,
		// we failed to register.
		if int(res.Status) >= int(swapserverrpc.HyperloopStatus_PUBLISHED) { //nolint:lll
			return false, errors.New("registration failed")
		}

		return false, nil
	}

	resultChan, errChan := f.waitForStateChan(ctx, checkFunc)

	// Dispatch the swap payment.
	paymentChan, payErrChan, err := f.cfg.Router.SendPayment(
		ctx, lndclient.SendPaymentRequest{
			Invoice: f.hyperloop.SwapInvoice,
			MaxFee:  getMaxRoutingFee(f.hyperloop.Amt),
			Timeout: defaultSendpaymentTimeout,
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	for {
		select {
		case <-ctx.Done():
			f.Debugf("Context canceled")
			return fsm.NoOp

		case err := <-payErrChan:
			f.Debugf("Payment error: %v", err)
			return f.HandleError(err)

		case err := <-errChan:
			f.Debugf("Error: %v", err)
			return f.HandleError(err)

		case res := <-paymentChan:
			f.Debugf("Payment result: %v", res)
			if res.State == lnrpc.Payment_FAILED {
				return f.HandleError(
					errors.New("payment failed"))
			}

		case <-resultChan:
			return OnRegistered
		}
	}
}

// waitForPublishAction is the action that waits for the hyperloop to be
// published.
func (f *FSM) waitForPublishAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	checkFunc := func(res *swapserverrpc.
		HyperloopNotificationStreamResponse) (bool, error) {

		if res.Status == swapserverrpc.HyperloopStatus_PUBLISHED ||
			res.Status == swapserverrpc.HyperloopStatus_WAIT_FOR_HTLC_NONCES { //nolint:lll

			return true, nil
		}

		return false, nil
	}

	err := f.waitForState(ctx, checkFunc)
	if err != nil {
		return f.HandleError(err)
	}

	p, err := f.cfg.HyperloopClient.FetchHyperloopParticipants(
		ctx,
		&swapserverrpc.FetchHyperloopParticipantsRequest{
			HyperloopId: f.hyperloop.ID[:],
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	// If we're published, the list of participants is final.
	participants, err := getParticipants(p.Participants, f.cfg.ChainParams)
	if err != nil {
		return f.HandleError(err)
	}

	f.ValLock.Lock()
	f.hyperloop.Participants = participants
	f.ValLock.Unlock()

	return OnPublished
}

// waitForConfirmationAction is the action that waits for the hyperloop to be
// confirmed.
func (f *FSM) waitForConfirmationAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	pkScript, err := f.hyperloop.getHyperLoopScript()
	if err != nil {
		return f.HandleError(err)
	}

	subChan, errChan, err := f.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		ctx, nil, pkScript, 2, f.hyperloop.InitiationHeight,
	)
	if err != nil {
		return f.HandleError(err)
	}

	for {
		select {
		case <-ctx.Done():
			return fsm.NoOp

		case err := <-errChan:
			return f.HandleError(err)

		case conf := <-subChan:
			f.hyperloop.ConfirmationHeight = int32(conf.BlockHeight)
			outpoint, amt, err := getOutpointFromTx(
				conf.Tx, pkScript,
			)
			if err != nil {
				return f.HandleError(err)
			}

			f.ValLock.Lock()
			f.hyperloop.ConfirmedOutpoint = outpoint
			f.hyperloop.ConfirmedValue = btcutil.Amount(amt)
			f.ValLock.Unlock()

			return OnConfirmed
		}
	}
}

// pushHtlcNonceAction is the action that pushes the htlc nonce to the server.
func (f *FSM) pushHtlcNonceAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	checkFunc := func(res *swapserverrpc.
		HyperloopNotificationStreamResponse) (bool, error) {
		// If the length of htlc nonces is equal to the
		// number of participants, we're ready to sign.
		return res.Status == swapserverrpc.HyperloopStatus_WAIT_FOR_HTLC_NONCES, // nolint:lll
			nil
	}

	err := f.waitForState(ctx, checkFunc)
	if err != nil {
		return f.HandleError(err)
	}

	// We'll first fetch the feeRates from the server.
	feeRates, err := f.cfg.HyperloopClient.FetchHyperloopHtlcFeeRates(
		ctx, &swapserverrpc.FetchHyperloopHtlcFeeRatesRequest{
			HyperloopId: f.hyperloop.ID[:],
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	f.Debugf("Fetched htlc fee rates: %v", feeRates.HtlcFeeRates)

	f.ValLock.Lock()
	defer f.ValLock.Unlock()
	f.hyperloop.setHtlcFeeRates(feeRates.HtlcFeeRates)

	err = f.hyperloop.startHtlcFeerateSessions(ctx, f.cfg.Signer)
	if err != nil {
		return f.HandleError(err)
	}

	nonceMap := f.hyperloop.getHtlcFeeRateMusig2Nonces()

	for feeRate, nonce := range nonceMap {
		f.Debugf("Pushing nonce for fee rate %v %v", feeRate,
			len(nonce))
	}

	res, err := f.cfg.HyperloopClient.PushHyperloopHtlcNonces(
		ctx, &swapserverrpc.PushHyperloopHtlcNoncesRequest{
			HyperloopId: f.hyperloop.ID[:],
			SwapHash:    f.hyperloop.SwapHash[:],
			HtlcNonces:  nonceMap,
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	for feeRate, nonce := range res.ServerHtlcNonces {
		var nonceBytes [musig2.PubNonceSize]byte
		copy(nonceBytes[:], nonce)

		f.hyperloop.HtlcServerNonces[feeRate] = nonceBytes
	}

	for feeRate, txBytes := range res.HtlcRawTxns {
		rawTx := &wire.MsgTx{}
		err := rawTx.Deserialize(bytes.NewReader(txBytes))
		if err != nil {
			return f.HandleError(err)
		}

		err = f.hyperloop.registerHtlcTx(
			f.cfg.ChainParams, feeRate, rawTx,
		)
		if err != nil {
			return f.HandleError(err)
		}
	}

	return OnPushedHtlcNonce
}

// waitForReadyForHtlcSignAction is the action that waits for the server to be
// ready for the htlc sign.
func (f *FSM) waitForReadyForHtlcSignAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	checkFunc := func(res *swapserverrpc.
		HyperloopNotificationStreamResponse) (bool, error) {
		// If the length of htlc nonces is equal to the
		// number of participants, we're ready to sign.
		return res.Status == swapserverrpc.HyperloopStatus_WAIT_FOR_HTLC_SIGS, // nolint:lll
			nil
	}

	err := f.waitForState(ctx, checkFunc)
	if err != nil {
		return f.HandleError(err)
	}

	res, err := f.cfg.HyperloopClient.FetchHyperloopHtlcNonces(
		ctx, &swapserverrpc.FetchHyperloopHtlcNoncesRequest{
			HyperloopId: f.hyperloop.ID[:],
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	f.ValLock.Lock()
	defer f.ValLock.Unlock()

	nonceMap := make(map[int64][][66]byte, len(res.HtlcNoncesByFees))
	for feeRate, htlcNonces := range res.HtlcNoncesByFees {
		nonceMap[feeRate] = make(
			[][66]byte, 0, len(htlcNonces.ParticipantNonce),
		)
		for _, htlcNonce := range htlcNonces.ParticipantNonce {
			htlcNonce := htlcNonce
			var nonce [66]byte
			copy(nonce[:], htlcNonce)
			nonceMap[feeRate] = append(nonceMap[feeRate], nonce)
		}
	}

	err = f.hyperloop.registerHtlcNonces(ctx, f.cfg.Signer, nonceMap)
	if err != nil {
		return f.HandleError(err)
	}

	return OnReadyForHtlcSig
}

// pushHtlcSigAction is the action that pushes the htlc signatures to the server.
func (f *FSM) pushHtlcSigAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	f.ValLock.Lock()
	defer f.ValLock.Unlock()

	htlcSigsMap := make(map[int64][]byte, len(f.hyperloop.HtlcRawTxns))
	f.Debugf("Pushing htlc sigs for raw txns: %v",
		len(f.hyperloop.HtlcRawTxns))
	for feeRate, htlcTx := range f.hyperloop.HtlcRawTxns {
		musigSession, ok := f.hyperloop.HtlcMusig2Sessions[feeRate]
		if !ok {
			return f.HandleError(errors.New(
				"no musig session found"))
		}
		sig, err := f.hyperloop.getSigForTx(
			ctx, f.cfg.Signer, htlcTx, musigSession.SessionID,
		)
		if err != nil {
			return f.HandleError(err)
		}
		htlcSigsMap[feeRate] = sig
	}

	_, err := f.cfg.HyperloopClient.PushHyperloopHtlcSigs(
		ctx, &swapserverrpc.PushHyperloopHtlcSigRequest{
			SwapHash:    f.hyperloop.SwapHash[:],
			HyperloopId: f.hyperloop.ID[:],
			HtlcSigs:    htlcSigsMap,
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	return OnPushedHtlcSig
}

// waitForHtlcSig is the action where we poll the server for the htlc signature.
func (f *FSM) waitForHtlcSig(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	checkFunc := func(res *swapserverrpc.
		HyperloopNotificationStreamResponse) (bool, error) {

		return res.Status == swapserverrpc.HyperloopStatus_WAIT_FOR_PREIMAGES, // nolint:lll
			nil
	}

	resChan, errChan := f.waitForStateChan(ctx, checkFunc)
	select {
	case <-ctx.Done():
		return fsm.NoOp

	case err := <-errChan:
		return f.HandleError(err)

	case <-resChan:

	// We'll also need to check if the hyperloop has been spent.
	case spend := <-f.spendChan:
		return f.getHyperloopSpendingEvent(spend)
	}

	res, err := f.cfg.HyperloopClient.FetchHyperloopHtlcSigs(
		ctx, &swapserverrpc.FetchHyperloopHtlcSigRequest{
			HyperloopId: f.hyperloop.ID[:],
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	f.ValLock.Lock()
	defer f.ValLock.Unlock()

	// Try to register the htlc sig, this will verify the sig is valid.
	for feeRate, htlcSig := range res.HtlcSigsByFees {
		err := f.hyperloop.registerHtlcSig(feeRate, htlcSig)
		if err != nil {
			return f.HandleError(err)
		}
	}

	return OnReceivedHtlcSig
}

// pushPreimageAction is the action that pushes the preimage to the server.
func (f *FSM) pushPreimageAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	checkFunc := func(res *swapserverrpc.
		HyperloopNotificationStreamResponse) (bool, error) {

		return res.Status == swapserverrpc.HyperloopStatus_WAIT_FOR_PREIMAGES, // nolint:lll
			nil
	}

	resChan, errChan := f.waitForStateChan(ctx, checkFunc)
	select {
	case <-ctx.Done():
		return fsm.NoOp

	case err := <-errChan:
		return f.HandleError(err)

	case <-resChan:

	// We'll also need to check if the hyperloop has been spent.
	case spend := <-f.spendChan:
		return f.getHyperloopSpendingEvent(spend)
	}

	// Start the sweep session.
	err := f.hyperloop.startSweeplessSession(ctx, f.cfg.Signer)
	if err != nil {
		return f.HandleError(err)
	}

	res, err := f.cfg.HyperloopClient.PushHyperloopPreimage(
		ctx, &swapserverrpc.PushHyperloopPreimageRequest{
			HyperloopId: f.hyperloop.ID[:],
			Preimage:    f.hyperloop.SwapPreimage[:],
			SweepNonce:  f.hyperloop.SweeplessSweepMusig2Session.PublicNonce[:], // nolint:lll
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	copy(f.hyperloop.SweepServerNonce[:], res.ServerSweepNonce)

	sweepTx := &wire.MsgTx{}
	err = sweepTx.Deserialize(bytes.NewReader(res.SweepRawTx))
	if err != nil {
		return f.HandleError(err)
	}

	// We need to fetch the full swap amount we're sweeping to our sweep
	// address.
	swapAmt, err := f.swapAmtFetcher.fetchHyperLoopTotalSweepAmt(
		f.hyperloop.ID, f.hyperloop.SweepAddr,
	)
	if err != nil {
		return f.HandleError(err)
	}

	err = f.hyperloop.registerSweeplessSweepTx(
		sweepTx, chainfee.SatPerKWeight(res.SweepFeeRate), swapAmt,
	)
	if err != nil {
		return f.HandleError(err)
	}

	return OnPushedPreimage
}

// waitForReadyForSweepAction is the action that waits for the server to be
// ready for the sweep.
func (f *FSM) waitForReadyForSweepAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	checkFunc := func(res *swapserverrpc.
		HyperloopNotificationStreamResponse) (bool, error) {

		return res.Status == swapserverrpc.HyperloopStatus_WAIT_FOR_SWEEPLESS_SWEEP_SIGS, // nolint:lll
			nil
	}

	resChan, errChan := f.waitForStateChan(ctx, checkFunc)

	select {
	case <-ctx.Done():
		return fsm.NoOp

	case err := <-errChan:
		return f.HandleError(err)

	case <-resChan:

	// We'll also need to check if the hyperloop has been spent.
	case spend := <-f.spendChan:
		return f.getHyperloopSpendingEvent(spend)
	}

	res, err := f.cfg.HyperloopClient.FetchHyperloopSweeplessSweepNonce(
		ctx, &swapserverrpc.FetchHyperloopSweeplessSweepNonceRequest{
			HyperloopId: f.hyperloop.ID[:],
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	var nonces [][66]byte
	for _, sweepNonce := range res.SweeplessSweepNonces {
		sweepNonce := sweepNonce

		var nonce [66]byte
		copy(nonce[:], sweepNonce)
		nonces = append(nonces, nonce)
	}

	err = f.hyperloop.registerSweeplessSweepNonces(
		ctx, f.cfg.Signer, nonces,
	)
	if err != nil {
		return f.HandleError(err)
	}

	return OnReadyForSweeplessSweepSig
}

func (f *FSM) pushSweepSigAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	sig, err := f.hyperloop.getSigForTx(
		ctx, f.cfg.Signer, f.hyperloop.SweeplessSweepRawTx,
		f.hyperloop.SweeplessSweepMusig2Session.SessionID,
	)
	if err != nil {
		return f.HandleError(err)
	}

	_, err = f.cfg.HyperloopClient.PushHyperloopSweeplessSweepSig(
		ctx, &swapserverrpc.PushHyperloopSweeplessSweepSigRequest{
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

func (f *FSM) waitForSweepPublishAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	select {
	case <-ctx.Done():
		return fsm.NoOp

	case spend := <-f.spendChan:
		return f.getHyperloopSpendingEvent(spend)
	}
}

// waitForSweeplessSweepConfirmationAction is the action that waits for the
// sweepless sweep to be confirmed.
func (f *FSM) waitForSweeplessSweepConfirmationAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	sweeplessSweepTxHash := f.hyperloop.SweeplessSweepRawTx.TxHash()
	pkscript, err := txscript.PayToAddrScript(f.hyperloop.SweepAddr)
	if err != nil {
		return f.HandleError(err)
	}

	confChan, errChan, err := f.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		ctx, &sweeplessSweepTxHash, pkscript, 2,
		f.hyperloop.ConfirmationHeight,
	)
	if err != nil {
		return f.HandleError(err)
	}

	for {
		select {
		case <-ctx.Done():
			return fsm.NoOp

		case err := <-errChan:
			return f.HandleError(err)

		case <-confChan:
			return OnSweeplessSweepConfirmed
		}
	}
}

// waitForState polls the server for the hyperloop status until the
// status matches the expected status. Once the status matches, it returns the
// response.
func (f *FSM) waitForState(ctx context.Context,
	checkFunc func(*swapserverrpc.HyperloopNotificationStreamResponse,
	) (bool, error)) error {

	// Create a context which we can cancel if we're published.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return errors.New("context canceled")

		case <-time.After(time.Second * 5):
			f.lastNotificationMutex.Lock()
			if f.lastNotification == nil {
				f.lastNotificationMutex.Unlock()
				continue
			}

			status, err := checkFunc(f.lastNotification)
			if err != nil {
				f.lastNotificationMutex.Unlock()
				return err
			}
			if status {
				f.lastNotificationMutex.Unlock()
				return nil
			}
			f.lastNotificationMutex.Unlock()
		}
	}
}

// waitForStateChan polls the server for the hyperloop status and returns
// channels for receiving the result or error.
func (f *FSM) waitForStateChan(
	ctx context.Context,
	checkFunc func(*swapserverrpc.HyperloopNotificationStreamResponse,
	) (bool, error),
) (chan *swapserverrpc.HyperloopNotificationStreamResponse, chan error) {

	resultChan := make(
		chan *swapserverrpc.HyperloopNotificationStreamResponse, 1,
	)
	errChan := make(chan error, 1)

	go func() {
		// Create a context which we can cancel if we're published.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		for {
			select {
			case <-ctx.Done():
				errChan <- errors.New("context canceled")
				return

			case <-time.After(time.Second):
				f.lastNotificationMutex.Lock()
				if f.lastNotification == nil {
					f.lastNotificationMutex.Unlock()
					continue
				}

				status, err := checkFunc(f.lastNotification)
				if err != nil {
					f.lastNotificationMutex.Unlock()
					errChan <- err
					return
				}

				if status {
					f.lastNotificationMutex.Unlock()
					resultChan <- f.lastNotification
					return
				}
				f.lastNotificationMutex.Unlock()
			}
		}
	}()

	return resultChan, errChan
}

// handleAsyncError is a helper method that logs an error and sends an error
// event to the FSM.
func (f *FSM) handleAsyncError(ctx context.Context, err error) {
	f.LastActionError = err
	f.Errorf("Error on async action: %v", err)
	err2 := f.SendEvent(ctx, fsm.OnError, err)
	if err2 != nil {
		f.Errorf("Error sending event: %v", err2)
	}
}

// getMaxRoutingFee returns the maximum routing fee for a given amount.
func getMaxRoutingFee(amt btcutil.Amount) btcutil.Amount {
	return swap.CalcFee(amt, maxRoutingFeeBase, maxRoutingFeeRate)
}

// getOutpointFromTx returns the outpoint and value of the pkScript in the tx.
func getOutpointFromTx(tx *wire.MsgTx, pkScript []byte) (*wire.OutPoint, int64,
	error) {

	for i, txOut := range tx.TxOut {
		if bytes.Equal(pkScript, txOut.PkScript) {
			txHash := tx.TxHash()

			return wire.NewOutPoint(&txHash, uint32(i)),
				txOut.Value, nil
		}
	}
	return nil, 0, errors.New("pk script not found in tx")
}

// getParticipants adds the participant public keys to the
// hyperloop.
func getParticipants(rpcParticipants []*swapserverrpc.HyperloopParticipant,
	chainParams *chaincfg.Params) ([]*HyperloopParticipant, error) {

	participants := make([]*HyperloopParticipant, 0, len(rpcParticipants))
	for _, p := range rpcParticipants {
		p := p
		pubkey, err := btcec.ParsePubKey(p.PublicKey)
		if err != nil {
			return nil, err
		}

		sweepAddr, err := btcutil.DecodeAddress(
			p.SweepAddr, chainParams,
		)
		if err != nil {
			return nil, err
		}

		participants = append(participants, &HyperloopParticipant{
			pubkey:       pubkey,
			sweepAddress: sweepAddr,
		})
	}

	return participants, nil
}

// getHyperloopSpendingEvent returns the event that should be triggered when the
// hyperloop output is spent. It returns an error if the spending transaction is
// unexpected.
func (f *FSM) getHyperloopSpendingEvent(spendDetails *chainntnfs.SpendDetail,
) fsm.EventType {

	spendingTxHash := spendDetails.SpendingTx.TxHash()

	// TODO: implement htlc spending
	// for _, htlcTx := range f.hyperloop.HtlcRawTxns {
	// 	htlcTxHash := htlcTx.TxHash()

	// 	if bytes.Equal(spendingTxHash[:], htlcTxHash[:]) {
	// 		return OnHtlcPublished
	// 	}
	// }

	sweeplessSweepTxHash := f.hyperloop.SweeplessSweepRawTx.TxHash()

	if bytes.Equal(spendingTxHash[:], sweeplessSweepTxHash[:]) {
		return OnSweeplessSweepPublish
	}

	return f.HandleError(errors.New("unexpected tx spent"))
}
