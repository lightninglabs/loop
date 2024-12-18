package deposit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	DefaultConfTarget = 3
)

// PublishDepositExpirySweepAction creates and publishes the timeout transaction
// that spends the deposit from the static address timeout leaf to the
// predefined timeout sweep pkscript.
func (f *FSM) PublishDepositExpirySweepAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	msgTx := wire.NewMsgTx(2)

	params, err := f.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return fsm.OnError
	}

	// Add the deposit outpoint as input to the transaction.
	msgTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: f.deposit.OutPoint,
		Sequence:         params.Expiry,
		SignatureScript:  nil,
	})

	// Estimate the fee rate of an expiry spend transaction.
	feeRateEstimator, err := f.cfg.WalletKit.EstimateFeeRate(
		ctx, DefaultConfTarget,
	)
	if err != nil {
		return f.HandleError(fmt.Errorf("timeout sweep fee "+
			"estimation failed: %w", err))
	}

	weight := script.ExpirySpendWeight()

	fee := feeRateEstimator.FeeForWeight(lntypes.WeightUnit(weight))

	// We cap the fee at 20% of the deposit value.
	if fee > f.deposit.Value/5 {
		return f.HandleError(errors.New("fee is greater than 20% of " +
			"the deposit value"))
	}

	output := &wire.TxOut{
		Value:    int64(f.deposit.Value - fee),
		PkScript: f.deposit.TimeOutSweepPkScript,
	}
	msgTx.AddTxOut(output)

	txOut := &wire.TxOut{
		Value:    int64(f.deposit.Value),
		PkScript: params.PkScript,
	}

	prevOut := []*wire.TxOut{txOut}

	signDesc, err := f.SignDescriptor(ctx)
	if err != nil {
		return f.HandleError(err)
	}

	rawSigs, err := f.cfg.Signer.SignOutputRaw(
		ctx, msgTx, []*lndclient.SignDescriptor{signDesc}, prevOut,
	)
	if err != nil {
		return f.HandleError(err)
	}

	address, err := f.cfg.AddressManager.GetStaticAddress(ctx)
	if err != nil {
		return f.HandleError(err)
	}

	sig := rawSigs[0]
	msgTx.TxIn[0].Witness, err = address.GenTimeoutWitness(sig)
	if err != nil {
		return f.HandleError(err)
	}

	txLabel := fmt.Sprintf("timeout sweep for deposit %v",
		f.deposit.OutPoint)

	err = f.cfg.WalletKit.PublishTransaction(ctx, msgTx, txLabel)
	if err != nil {
		if !strings.Contains(err.Error(), "output already spent") {
			log.Errorf("%v: %v", txLabel, err)
			f.LastActionError = err
			return fsm.OnError
		}
	} else {
		f.Debugf("published timeout sweep with txid: %v",
			msgTx.TxHash())
	}

	return OnExpiryPublished
}

// WaitForExpirySweepAction waits for a sufficient number of confirmations
// before a timeout sweep is considered successful.
func (f *FSM) WaitForExpirySweepAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	spendChan, errSpendChan, err := f.cfg.ChainNotifier.RegisterConfirmationsNtfn( //nolint:lll
		ctx, nil, f.deposit.TimeOutSweepPkScript, DefaultConfTarget,
		int32(f.deposit.ConfirmationHeight),
	)
	if err != nil {
		return f.HandleError(err)
	}

	select {
	case err = <-errSpendChan:
		log.Debugf("error while sweeping expired deposit: %v", err)
		return fsm.OnError

	case confirmedTx := <-spendChan:
		f.deposit.ExpirySweepTxid = confirmedTx.Tx.TxHash()
		return OnExpirySwept

	case <-ctx.Done():
		return fsm.OnError
	}
}

// SweptExpiredDepositAction is the final action of the FSM. It signals to the
// manager that the deposit has been swept and the FSM can be removed. It also
// ends the state machine main loop by cancelling its context.
func (f *FSM) SweptExpiredDepositAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	select {
	case <-ctx.Done():
		return fsm.OnError

	default:
		f.finalizedDepositChan <- f.deposit.OutPoint
		ctx.Done()
	}

	return fsm.NoOp
}

// FinalizeDepositAction is the final action after a withdrawal. It signals to
// the manager that the deposit has been swept and the FSM can be removed.
func (f *FSM) FinalizeDepositAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	select {
	case <-ctx.Done():
		return fsm.OnError

	default:
		f.finalizedDepositChan <- f.deposit.OutPoint
	}

	return fsm.NoOp
}
