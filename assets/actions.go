package assets

import (
	"context"
	"crypto/rand"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

// InitSwapOutContext is the initial context for the InitSwapOut state.
type InitSwapOutContext struct {
	// Amount is the amount of the swap.
	Amount uint64
	// AssetId is the id of the asset we are swapping.
	AssetId []byte
	// BlockheightHint is the hint for the current block height.
	BlockHeightHint uint32
}

// InitSwapOut is the first state of the swap out FSM. It is responsible for
// creating a new swap out and prepay invoice.
func (o *OutFSM) InitSwapOut(ctx context.Context,
	initCtx fsm.EventContext) fsm.EventType {

	// We expect the event context to be of type *InstantOutContext.
	req, ok := initCtx.(*InitSwapOutContext)
	if !ok {
		o.Errorf("expected InstantOutContext, got %T", initCtx)
		return o.HandleError(fsm.ErrInvalidContextType)
	}

	// Create a new key for the swap.
	clientKeyDesc, err := o.cfg.Wallet.DeriveNextKey(
		ctx, AssetKeyFamily,
	)
	if err != nil {
		return o.HandleError(err)
	}

	// Request the asset out.
	assetOutRes, err := o.cfg.AssetClient.RequestAssetLoopOut(
		ctx, &swapserverrpc.RequestAssetLoopOutRequest{
			Amount:         req.Amount,
			RequestedAsset: req.AssetId,
			ReceiverKey:    clientKeyDesc.PubKey.SerializeCompressed(),
		},
	)
	if err != nil {
		return o.HandleError(err)
	}

	// Create the swap hash from the response.
	swapHash, err := lntypes.MakeHash(assetOutRes.SwapHash)
	if err != nil {
		return o.HandleError(err)
	}

	// Parse the server pubkey.
	senderPubkey, err := btcec.ParsePubKey(assetOutRes.SenderPubkey)
	if err != nil {
		return o.HandleError(err)
	}

	// With our params, we'll now create the swap out.
	swapOut := NewSwapOut(
		swapHash, req.Amount,
		req.AssetId, clientKeyDesc, senderPubkey,
		uint32(assetOutRes.Expiry), req.BlockHeightHint,
	)
	o.SwapOut = swapOut
	o.PrepayInvoice = assetOutRes.PrepayInvoice

	err = o.cfg.Store.CreateAssetSwapOut(ctx, o.SwapOut)
	if err != nil {
		return o.HandleError(err)
	}

	return onAssetOutInit
}

// PayPrepay is the state where we try to pay the prepay invoice.
func (o *OutFSM) PayPrepay(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	trackChan, errChan, err := o.cfg.TapdClient.SendPayment(
		ctx, o.PrepayInvoice, o.SwapOut.AssetID,
	)
	if err != nil {
		return o.HandleError(err)
	}

	go func() {
		for {
			select {
			case result := <-trackChan:
				if result.GetAcceptedSellOrder() != nil {
					o.Debugf("accepted prepay sell order")
				}
				if payRes := result.GetPaymentResult(); payRes != nil {
					if payRes.GetFailureReason() !=
						lnrpc.PaymentFailureReason_FAILURE_REASON_NONE {
						o.Errorf("payment failed: %v", payRes.FailureReason)
						err = o.SendEvent(ctx, fsm.OnError, nil)
						if err != nil {
							o.Errorf("unable to send event: %v", err)
						}
						return
					}
					if payRes.Status == lnrpc.Payment_SUCCEEDED {
						o.Debugf("payment succeeded")
						err := o.SendEvent(ctx, onPrepaySettled, nil)
						if err != nil {
							o.Errorf("unable to send event: %v", err)
						}
						return
					}
					if payRes.Status == lnrpc.Payment_IN_FLIGHT {
						o.Debugf("payment in flight")
					}
				}

			case err := <-errChan:
				o.Errorf("payment error: %v", err)
				err = o.SendEvent(ctx, fsm.OnError, nil)
				if err != nil {
					o.Errorf("unable to send event: %v", err)
				}
				return

			case <-ctx.Done():
				return
			}
		}
	}()

	return fsm.NoOp
}

// FetchProof is the state where we fetch the proof.
func (o *OutFSM) FetchProof(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	// Fetch the proof from the server.
	proofRes, err := o.cfg.AssetClient.PollAssetLoopOutProof(
		ctx, &swapserverrpc.PollAssetLoopOutProofRequest{
			SwapHash: o.SwapOut.SwapHash[:],
		},
	)
	// If we have an error, we'll wait for the next block and try again.
	if err != nil {
		return onWaitForBlock
	}

	// We'll now import the proof into the asset client.
	_, err = o.cfg.TapdClient.ImportProofFile(
		ctx, proofRes.RawProofFile,
	)
	if err != nil {
		return o.HandleError(err)
	}

	o.SwapOut.RawHtlcProof = proofRes.RawProofFile

	// We'll now save the proof in the database.
	err = o.cfg.Store.UpdateAssetSwapOutProof(
		ctx, o.SwapOut.SwapHash, proofRes.RawProofFile,
	)
	if err != nil {
		return o.HandleError(err)
	}

	return onProofReceived
}

func (o *OutFSM) waitForBlock(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	blockHeight := o.cfg.BlockHeightSubscriber.GetBlockHeight()

	cb := func() {
		err := o.SendEvent(ctx, onBlockReceived, nil)
		if err != nil {
			log.Errorf("Error sending block event %w", err)
		}
	}

	subscriberId, err := getRandomHash()
	if err != nil {
		return o.HandleError(err)
	}

	alreadyPassed := o.cfg.BlockHeightSubscriber.SubscribeExpiry(
		subscriberId, blockHeight+1, cb,
	)
	if alreadyPassed {
		return onBlockReceived
	}

	return fsm.NoOp
}

// subscribeToHtlcTxConfirmed is the state where we subscribe to the htlc
// transaction to wait for it to be confirmed.
//
// Todo(sputn1ck): handle rebroadcasting if it doesn't confirm.
func (o *OutFSM) subscribeToHtlcTxConfirmed(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	// First we'll get the htlc pkscript.
	htlcPkScript, err := o.getHtlcPkscript()
	if err != nil {
		return o.HandleError(err)
	}

	o.Debugf("pkscript: %x", htlcPkScript)

	txConfCtx, cancel := context.WithCancel(ctx)

	confCallback := func(conf *chainntnfs.TxConfirmation, err error) {
		if err != nil {
			o.LastActionError = err
			err = o.SendEvent(ctx, fsm.OnError, nil)
			if err != nil {
				log.Errorf("Error sending block event %w", err)
			}
		}
		cancel()
		err = o.SendEvent(ctx, onHtlcTxConfirmed, conf)
		if err != nil {
			log.Errorf("Error sending block event %w", err)
		}
	}

	err = o.cfg.TxConfSubscriber.SubscribeTxConfirmation(
		txConfCtx, o.SwapOut.SwapHash, nil,
		htlcPkScript, defaultHtlcConfRequirement,
		int32(o.SwapOut.InitiationHeight), confCallback,
	)
	if err != nil {
		return o.HandleError(err)
	}

	return fsm.NoOp
}

// sendSwapPayment is the state where we pay the swap invoice.
func (o *OutFSM) sendSwapPayment(ctx context.Context,
	event fsm.EventContext) fsm.EventType {

	// If we have an EventContext with a confirmation, we'll save the
	// confirmation height.
	if event != nil {
		if conf, ok := event.(*chainntnfs.TxConfirmation); ok {
			outpoint, err := o.findPkScript(conf.Tx)
			if err != nil {
				return o.HandleError(err)
			}
			o.SwapOut.HtlcConfirmationHeight = conf.BlockHeight
			o.SwapOut.HtlcOutPoint = outpoint

			err = o.cfg.Store.UpdateAssetSwapHtlcOutpoint(
				ctx, o.SwapOut.SwapHash,
				outpoint, int32(conf.BlockHeight),
			)
			if err != nil {
				o.Errorf(
					"unable to update swap outpoint: %v",
					err,
				)
			}
		}
	}

	// Fetch the proof from the server.
	buyRes, err := o.cfg.AssetClient.RequestAssetBuy(
		ctx, &swapserverrpc.RequestAssetBuyRequest{
			SwapHash: o.SwapOut.SwapHash[:],
		},
	)
	if err != nil {
		return o.HandleError(err)
	}

	// We'll also set the swap invoice.
	o.SwapInvoice = buyRes.SwapInvoice

	// If the htlc has been confirmed, we can now pay the swap invoice.
	trackChan, errChan, err := o.cfg.TapdClient.SendPayment(
		ctx, o.SwapInvoice, o.SwapOut.AssetID,
	)
	if err != nil {
		return o.HandleError(err)
	}

	go func() {
		for {
			select {
			case result := <-trackChan:
				if result.GetAcceptedSellOrder() != nil {
					o.Debugf("swpa invoice accepted swap" +
						"sell order")
				}
				if payRes := result.GetPaymentResult(); payRes != nil {
					payStatus := payRes.GetStatus()
					if payStatus == lnrpc.Payment_FAILED {
						o.Errorf("payment failed: %v", payRes.FailureReason)
						err = o.SendEvent(ctx, fsm.OnError, nil)
						if err != nil {
							o.Errorf("unable to send event: %v", err)
						}
						return
					}
					if payStatus == lnrpc.Payment_SUCCEEDED {
						preimage, err := lntypes.MakePreimageFromStr(
							payRes.PaymentPreimage,
						)
						if err != nil {
							o.Errorf("unable to make preimage: %v", err)
						}
						o.SwapOut.SwapPreimage = preimage
						err = o.cfg.Store.UpdateAssetSwapOutPreimage(
							ctx, o.SwapOut.SwapHash,
							preimage,
						)
						if err != nil {
							o.Errorf(
								"unable to update swap preimage: %v",
								err,
							)
						}
						err = o.SendEvent(ctx, onSwapPreimageReceived, nil)
						if err != nil {
							o.Errorf("unable to send event: %v", err)
						}
						return
					}
				}

			case err := <-errChan:
				o.Errorf("payment error: %v", err)
				err = o.SendEvent(ctx, fsm.OnError, nil)
				if err != nil {
					o.Errorf("unable to send event: %v", err)
				}
				return

			case <-ctx.Done():
				return
			}
		}
	}()

	return fsm.NoOp
}

// publishSweepTx is the state where we publish the timeout transaction.
func (o *OutFSM) publishSweepTx(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	// Create the sweep address.
	rpcSweepAddr, err := o.cfg.TapdClient.NewAddr(
		ctx, &taprpc.NewAddrRequest{
			AssetId: o.SwapOut.AssetID,
			Amt:     o.SwapOut.Amount,
		},
	)
	if err != nil {
		return o.HandleError(err)
	}

	sweepAddr, err := address.DecodeAddress(
		rpcSweepAddr.Encoded, o.cfg.AddrParams,
	)
	if err != nil {
		return o.HandleError(err)
	}

	// Publish and log the sweep transaction.
	outpoint, pkScript, err := o.publishPreimageSweep(ctx, sweepAddr)
	if err != nil {
		return o.HandleError(err)
	}

	o.SwapOut.SweepOutpoint = outpoint
	o.SwapOut.SweepPkscript = pkScript

	// We can now save the swap outpoint.
	err = o.cfg.Store.UpdateAssetSwapOutSweepTx(
		ctx, o.SwapOut.SwapHash, outpoint.Hash,
		0, pkScript,
	)
	if err != nil {
		return o.HandleError(err)
	}

	return onHtlcSuccessSweep
}

// subscribeSweepConf is the state where we subscribe to the sweep transaction
// confirmation.
func (o *OutFSM) subscribeSweepConf(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	// We'll now subscribe to the confirmation of the sweep transaction.
	txConfCtx, cancel := context.WithCancel(ctx)

	confCallback := func(conf *chainntnfs.TxConfirmation, err error) {
		if err != nil {
			o.LastActionError = err
			err = o.SendEvent(ctx, fsm.OnError, nil)
			if err != nil {
				o.Errorf("Error sending conf event %w", err)
			}
		}
		cancel()
		err = o.SendEvent(ctx, onSweepTxConfirmed, conf)
		if err != nil {
			o.Errorf("Error sending conf event %w", err)
		}
	}

	err := o.cfg.TxConfSubscriber.SubscribeTxConfirmation(
		txConfCtx, o.SwapOut.SwapHash,
		&o.SwapOut.SweepOutpoint.Hash, o.SwapOut.SweepPkscript,
		defaultHtlcConfRequirement, int32(o.SwapOut.InitiationHeight),
		confCallback,
	)
	if err != nil {
		return o.HandleError(err)
	}

	return fsm.NoOp
}

// HandleError is a helper function that can be used by actions to handle
// errors.
func (o *OutFSM) HandleError(err error) fsm.EventType {
	if o == nil {
		log.Errorf("StateMachine error: %s", err)
		return fsm.OnError
	}
	o.Errorf("StateMachine error: %s", err)
	o.LastActionError = err
	return fsm.OnError
}

// getRandomHash returns a random hash.
func getRandomHash() (lntypes.Hash, error) {
	var preimage lntypes.Preimage
	_, err := rand.Read(preimage[:])
	if err != nil {
		return lntypes.Hash{}, err
	}

	return preimage.Hash(), nil
}
