package assets

import (
	"bytes"
	"context"
	"errors"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// Limit the observers transition observation stack to 15 entries.
	defaultObserverSize = 15
)

// States.
const (
	// Init is the initial state of the swap.
	Init fsm.StateType = "Init"

	// PayPrepay is the state where we are waiting for the
	// prepay invoice to be accepted.
	PayPrepay fsm.StateType = "PayPrepay"

	// FetchProof is the state where the prepay invoice has been
	// accepted.
	FetchProof fsm.StateType = "FetchProof"

	// WaitForBlock is the state where we are waiting for the next block
	// to be mined.
	WaitForBlock fsm.StateType = "WaitForBlock"

	// WaitForHtlcConfirmed is the state where the htlc transaction
	// has been broadcast.
	WaitForHtlcConfirmed fsm.StateType = "WaitForHtlcConfirmed"

	// HtlcTxConfirmed is the state where the htlc transaction
	// has been confirmed.
	HtlcTxConfirmed fsm.StateType = "HtlcTxConfirmed"

	// SweepHtlc is the state where we are creating the swap
	// invoice.
	SweepHtlc fsm.StateType = "SweepHtlc"

	// WaitForSweepConfirmed is the state where we are waiting for the swap
	// payment to be made. This is after we have given the receiver the
	// taproot assets proof.
	WaitForSweepConfirmed fsm.StateType = "WaitForSweepConfirmed"

	// Finished is the state where the swap has finished.
	Finished fsm.StateType = "Finished"

	// FinishedTimeout is the state where the swap has finished due to
	// a timeout.
	FinishedTimeout fsm.StateType = "FinishedTimeout"

	// Failed is the state where the swap has failed.
	Failed fsm.StateType = "Failed"
)

var (
	finishedStates = []fsm.StateType{
		Finished, FinishedTimeout, Failed,
	}
)

// Events.
var (
	// OnRequestAssetOut is the event where the server receives a swap
	// request from the client.
	OnRequestAssetOut = fsm.EventType("OnRequestAssetOut")

	// onAssetOutInit is the event where the server has initialized the
	// swap.
	onAssetOutInit = fsm.EventType("OnAssetOutInit")

	// onPrepaySettled is the event where the prepay invoice has been
	// accepted.
	onPrepaySettled = fsm.EventType("onPrepaySettled")

	onWaitForBlock = fsm.EventType("onWaitForBlock")

	onBlockReceived = fsm.EventType("onBlockReceived")

	onProofReceived = fsm.EventType("OnProofReceived")

	onHtlcTxConfirmed = fsm.EventType("onHtlcTxConfirmed")

	onSwapPreimageReceived = fsm.EventType("OnSwapPreimageReceived")

	// onHtlcSuccessSweep is the event where the htlc has timed out and we
	// are trying to sweep the htlc output.
	onHtlcSuccessSweep = fsm.EventType("onHtlcSuccessSweep")

	// onSweepTxConfirmed is the event where the sweep transaction has been
	// confirmed.
	onSweepTxConfirmed = fsm.EventType("OnSweepTxConfirmed")

	// OnRecover is the event where the swap is being recovered.
	OnRecover = fsm.EventType("OnRecover")
)

// FSMConfig contains the configuration for the FSM.
type FSMConfig struct {
	// TapdClient is the client to interact with the taproot asset daemon.
	TapdClient AssetClient

	// AssetClient is the client to interact with the asset swap server.
	AssetClient swapserverrpc.AssetsSwapServerClient

	// BlockHeightSubscriber is the subscriber to the block height.
	BlockHeightSubscriber BlockHeightSubscriber

	// TxConfSubscriber is the subscriber to the transaction confirmation.
	TxConfSubscriber TxConfirmationSubscriber

	// ExchangeRateProvider is the provider for the exchange rate.
	ExchangeRateProvider ExchangeRateProvider

	// Wallet is the wallet client.
	Wallet lndclient.WalletKitClient

	// Signer is the signer client.
	Signer lndclient.SignerClient

	// Store is the swap store.
	Store SwapStore

	// AddrParams are the chain parameters for addresses.
	AddrParams *address.ChainParams
}

type OutFSM struct {
	*fsm.StateMachine

	cfg *FSMConfig

	// SwapOut contains all the information about the swap.
	SwapOut *SwapOut

	// PrepayInvoice is the prepay invoice that we are paying to initiate
	// the swap.
	PrepayInvoice string

	// SwapInvoice is the swap invoice that we are sending to the receiver.
	SwapInvoice string

	// HtlcProof is the htlc proof that we use to sweep the htlc output.
	HtlcProof *proof.Proof
}

// NewOutFSM creates a new OutFSM.
func NewOutFSM(cfg *FSMConfig) *OutFSM {
	out := &SwapOut{
		State: fsm.EmptyState,
	}

	return NewOutFSMFromSwap(cfg, out)
}

// NewOutFSMFromSwap creates a new OutFSM from a existing swap.
func NewOutFSMFromSwap(cfg *FSMConfig, swap *SwapOut,
) *OutFSM {

	outFSM := &OutFSM{
		cfg:     cfg,
		SwapOut: swap,
	}

	outFSM.StateMachine = fsm.NewStateMachineWithState(
		outFSM.GetStates(), outFSM.SwapOut.State, defaultObserverSize,
	)
	outFSM.ActionEntryFunc = outFSM.updateSwap

	return outFSM
}

// GetStates returns the swap out state machine.
func (o *OutFSM) GetStates() fsm.States {
	return fsm.States{
		fsm.EmptyState: fsm.State{
			Transitions: fsm.Transitions{
				OnRequestAssetOut: Init,
			},
			Action: nil,
		},
		Init: fsm.State{
			Transitions: fsm.Transitions{
				onAssetOutInit: PayPrepay,
				// Before the htlc has been signed we can always
				// fail the swap.
				OnRecover:   Failed,
				fsm.OnError: Failed,
			},
			Action: o.InitSwapOut,
		},
		PayPrepay: fsm.State{
			Transitions: fsm.Transitions{
				onPrepaySettled: FetchProof,
				fsm.OnError:     Failed,
				OnRecover:       Failed,
			},
			Action: o.PayPrepay,
		},
		FetchProof: fsm.State{
			Transitions: fsm.Transitions{
				onWaitForBlock:  WaitForBlock,
				onProofReceived: WaitForHtlcConfirmed,
				fsm.OnError:     Failed,
				OnRecover:       FetchProof,
			},
			Action: o.FetchProof,
		},
		WaitForBlock: fsm.State{
			Transitions: fsm.Transitions{
				onBlockReceived: FetchProof,
				OnRecover:       FetchProof,
			},
			Action: o.waitForBlock,
		},
		WaitForHtlcConfirmed: fsm.State{
			Transitions: fsm.Transitions{
				onHtlcTxConfirmed: HtlcTxConfirmed,
				fsm.OnError:       Failed,
				OnRecover:         WaitForHtlcConfirmed,
			},
			Action: o.subscribeToHtlcTxConfirmed,
		},
		HtlcTxConfirmed: fsm.State{
			Transitions: fsm.Transitions{
				onSwapPreimageReceived: SweepHtlc,
				// Todo(sputn1ck) change to wait for expiry state.
				fsm.OnError: Failed,
				OnRecover:   HtlcTxConfirmed,
			},
			Action: o.sendSwapPayment,
		},
		SweepHtlc: fsm.State{
			Transitions: fsm.Transitions{
				onHtlcSuccessSweep: WaitForSweepConfirmed,
				fsm.OnError:        SweepHtlc,
				OnRecover:          SweepHtlc,
			},
			Action: o.publishSweepTx,
		},
		WaitForSweepConfirmed: fsm.State{
			Transitions: fsm.Transitions{
				onSweepTxConfirmed: Finished,
				fsm.OnError:        WaitForSweepConfirmed,
				OnRecover:          WaitForSweepConfirmed,
			},
			Action: o.subscribeSweepConf,
		},
		Finished: fsm.State{
			Action: fsm.NoOpAction,
		},
		Failed: fsm.State{
			Action: fsm.NoOpAction,
		},
	}
}

// // getSwapCopy returns a copy of the swap that is safe to be used from the
// // caller.
//  func (o *OutFSM) getSwapCopy() *SwapOut {

// updateSwap is called after every action and updates the swap in the db.
func (o *OutFSM) updateSwap(ctx context.Context,
	notification fsm.Notification) {

	// Skip the update if the swap is not yet initialized.
	if o.SwapOut == nil {
		return
	}

	o.Infof("Current: %v", notification.NextState)

	o.SwapOut.State = notification.NextState

	// If we're in the early stages we don't have created the swap in the
	// store yet and won't need to update it.
	if o.SwapOut.State == Init || (notification.PreviousState == Init &&
		notification.NextState == Failed) {

		return
	}

	err := o.cfg.Store.InsertAssetSwapUpdate(
		ctx, o.SwapOut.SwapHash, o.SwapOut.State,
	)
	if err != nil {
		log.Errorf("Error updating swap : %v", err)
		return
	}
}

// getHtlcPkscript returns the pkscript of the htlc output.
func (o *OutFSM) getHtlcPkscript() ([]byte, error) {
	// First fetch the proof.
	proof, err := o.getHtlcProof()
	if err != nil {
		return nil, err
	}

	// // Verify that the asset script matches the one predicted.
	// assetScriptkey, _, _, _, err := createOpTrueLeaf()
	// if err != nil {
	// 	return nil, err
	// }

	// o.Debugf("Asset script key: %x", assetScriptkey.PubKey.SerializeCompressed())
	// o.Debugf("Proof script key: %x", proof.Asset.ScriptKey.PubKey.SerializeCompressed())
	// if !bytes.Equal(
	// 	proof.Asset.ScriptKey.PubKey.SerializeCompressed(),
	// 	assetScriptkey.PubKey.SerializeCompressed(),
	// ) {
	// 	return nil, fmt.Errorf("asset script key mismatch")
	// }

	assetCpy := proof.Asset.Copy()
	assetCpy.PrevWitnesses[0].SplitCommitment = nil
	sendCommitment, err := commitment.NewAssetCommitment(
		assetCpy,
	)
	if err != nil {
		return nil, err
	}

	version := commitment.TapCommitmentV2
	assetCommitment, err := commitment.NewTapCommitment(
		&version, sendCommitment,
	)
	if err != nil {
		return nil, err
	}

	siblingPreimage, err := o.SwapOut.GetSiblingPreimage()
	if err != nil {
		return nil, err
	}

	siblingHash, err := siblingPreimage.TapHash()
	if err != nil {
		return nil, err
	}

	btcInternalKey, err := o.SwapOut.GetAggregateKey()
	if err != nil {
		return nil, err
	}

	anchorPkScript, err := tapscript.PayToAddrScript(
		*btcInternalKey, siblingHash, *assetCommitment,
	)
	if err != nil {
		return nil, err
	}

	return anchorPkScript, nil
}

// publishPreimageSweep publishes and logs the preimage sweep transaction.
func (o *OutFSM) publishPreimageSweep(ctx context.Context,
	addr *address.Tap) (*wire.OutPoint, []byte, error) {

	// Check if we have the proof in memory.
	htlcProof, err := o.getHtlcProof()
	if err != nil {
		return nil, nil, err
	}

	sweepVpkt, err := CreateOpTrueSweepVpkt(
		ctx, []*proof.Proof{htlcProof}, addr, o.cfg.AddrParams,
	)
	if err != nil {
		return nil, nil, err
	}

	feeRate, err := o.cfg.Wallet.EstimateFeeRate(
		ctx, defaultHtlcFeeConfTarget,
	)
	if err != nil {
		return nil, nil, err
	}

	// We'll now commit the vpkt in the btcpacket.
	sweepBtcPacket, activeAssets, passiveAssets, commitResp, err :=
		o.cfg.TapdClient.PrepareAndCommitVirtualPsbts(
			// todo check change desc and params.
			ctx, sweepVpkt, feeRate.FeePerVByte(), nil, nil,
		)
	if err != nil {
		return nil, nil, err
	}

	witness, err := o.createPreimageWitness(
		ctx, sweepBtcPacket, htlcProof,
	)
	if err != nil {
		return nil, nil, err
	}

	var buf bytes.Buffer
	err = psbt.WriteTxWitness(&buf, witness)
	if err != nil {
		return nil, nil, err
	}
	sweepBtcPacket.Inputs[0].SighashType = txscript.SigHashDefault
	sweepBtcPacket.Inputs[0].FinalScriptWitness = buf.Bytes()

	signedBtcPacket, err := o.cfg.Wallet.SignPsbt(ctx, sweepBtcPacket)
	if err != nil {
		return nil, nil, err
	}

	finalizedBtcPacket, _, err := o.cfg.Wallet.FinalizePsbt(
		ctx, signedBtcPacket, "",
	)
	if err != nil {
		return nil, nil, err
	}

	pkScript := finalizedBtcPacket.UnsignedTx.TxOut[0].PkScript

	// Now we'll publish and log the transfer.
	sendResp, err := o.cfg.TapdClient.LogAndPublish(
		ctx, finalizedBtcPacket, activeAssets, passiveAssets,
		commitResp,
	)
	if err != nil {
		return nil, nil, err
	}

	sweepAnchor := sendResp.Transfer.Outputs[0].Anchor

	outPoint, err := wire.NewOutPointFromString(sweepAnchor.Outpoint)
	if err != nil {
		return nil, nil, err
	}

	return outPoint, pkScript, nil
}

// getHtlcProof returns the htlc proof for the swap. If the proof is not
// in memory, we will recreate it from the stored proof file.
func (o *OutFSM) getHtlcProof() (*proof.Proof, error) {
	// Check if we have the proof in memory.
	if o.HtlcProof != nil {
		return o.HtlcProof, nil
	}

	// Parse the proof.
	htlcProofFile, err := proof.DecodeFile(o.SwapOut.RawHtlcProof)
	if err != nil {
		return nil, err
	}

	// Get the proofs.
	htlcProof, err := htlcProofFile.LastProof()
	if err != nil {
		return nil, err
	}

	return htlcProof, nil
}

// createPreimageWitness creates a preimage witness for the swap.
func (o *OutFSM) createPreimageWitness(ctx context.Context,
	sweepBtcPacket *psbt.Packet, htlcProof *proof.Proof) (wire.TxWitness,
	error) {

	assetTxOut := &wire.TxOut{
		PkScript: sweepBtcPacket.Inputs[0].WitnessUtxo.PkScript,
		Value:    sweepBtcPacket.Inputs[0].WitnessUtxo.Value,
	}
	feeTxOut := &wire.TxOut{
		PkScript: sweepBtcPacket.Inputs[1].WitnessUtxo.PkScript,
		Value:    sweepBtcPacket.Inputs[1].WitnessUtxo.Value,
	}

	sweepBtcPacket.UnsignedTx.TxIn[0].Sequence = 1

	successScript, err := o.SwapOut.GetSuccessScript()
	if err != nil {
		return nil, err
	}

	signDesc := &lndclient.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: o.SwapOut.ClientKeyLocator,
		},
		SignMethod:    input.TaprootScriptSpendSignMethod,
		WitnessScript: successScript,
		Output:        assetTxOut,
		InputIndex:    0,
	}
	sig, err := o.cfg.Signer.SignOutputRaw(
		ctx, sweepBtcPacket.UnsignedTx, []*lndclient.SignDescriptor{signDesc},
		[]*wire.TxOut{assetTxOut, feeTxOut},
	)
	if err != nil {
		return nil, err
	}
	taprootAssetRoot, err := o.SwapOut.genTaprootAssetRootFromProof(
		htlcProof,
	)
	if err != nil {
		return nil, err
	}
	successControlBlock, err := o.SwapOut.GenSuccessBtcControlBlock(
		taprootAssetRoot,
	)
	if err != nil {
		return nil, err
	}
	controlBlockBytes, err := successControlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return wire.TxWitness{
		o.SwapOut.SwapPreimage[:],
		sig[0],
		successScript,
		controlBlockBytes,
	}, nil
}

// Infof logs an info message with the swap hash as prefix.
func (o *OutFSM) Infof(format string, args ...interface{}) {
	log.Infof(
		"Swap %v: "+format,
		append(
			[]interface{}{o.SwapOut.SwapHash},
			args...,
		)...,
	)
}

// Debugf logs a debug message with the swap hash as prefix.
func (o *OutFSM) Debugf(format string, args ...interface{}) {
	log.Debugf(
		"Swap %v: "+format,
		append(
			[]interface{}{o.SwapOut.SwapHash},
			args...,
		)...,
	)
}

// Errorf logs an error message with the swap hash as prefix.
func (o *OutFSM) Errorf(format string, args ...interface{}) {
	log.Errorf(
		"Swap %v: "+format,
		append(
			[]interface{}{o.SwapOut.SwapHash},
			args...,
		)...,
	)
}
func (o *OutFSM) findPkScript(tx *wire.MsgTx) (*wire.OutPoint,
	error) {

	pkScript, err := o.getHtlcPkscript()
	if err != nil {
		return nil, err
	}

	for i, out := range tx.TxOut {
		if bytes.Equal(out.PkScript, pkScript) {
			txHash := tx.TxHash()
			return wire.NewOutPoint(&txHash, uint32(i)), nil
		}
	}
	return nil, errors.New("pkscript not found")
}

// IsFinishedState returns true if the passed state is a finished state.
func IsFinishedState(state fsm.StateType) bool {
	for _, s := range finishedStates {
		if s == state {
			return true
		}
	}

	return false
}

// FinishedStates returns a string slice of all finished states.
func FinishedStates() []string {
	states := make([]string, 0, len(finishedStates))
	for _, s := range finishedStates {
		states = append(states, string(s))
	}

	return states
}
