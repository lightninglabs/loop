package deposit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
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

	if f.deposit.AddressParams == nil {
		return f.HandleError(fmt.Errorf("missing static address " +
			"parameters"))
	}
	params := f.deposit.AddressParams

	address, err := f.deposit.GetStaticAddressScript()
	if err != nil {
		return f.HandleError(err)
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

	minRelayFeeRate, err := f.cfg.WalletKit.MinRelayFee(ctx)
	if err != nil {
		return f.HandleError(fmt.Errorf("timeout sweep min relay "+
			"query failed: %w", err))
	}

	weight := script.ExpirySpendWeight()

	fee := feeRateEstimator.FeeForWeight(lntypes.WeightUnit(weight))

	// We cap the fee at 20% of the deposit value.
	_, clamped, err := utils.ClampSweepFee(
		fee, f.deposit.Value, utils.MaxFeeToAmountRatio,
		minRelayFeeRate, lntypes.WeightUnit(weight),
	)
	if err != nil {
		return f.HandleError(err)
	}
	if clamped {
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
		txHash := msgTx.TxHash()
		f.deposit.ExpirySweepTxid = txHash
		f.Debugf("published timeout sweep with txid: %v", txHash)
	}

	return OnExpiryPublished
}

// WaitForExpirySweepAction waits for enough confirmations before a timeout
// sweep is considered successful.
func (f *FSM) WaitForExpirySweepAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	if f.deposit.AddressParams == nil {
		return f.HandleError(fmt.Errorf("missing static address " +
			"parameters"))
	}

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()

	var (
		spendChan    chan *chainntnfs.SpendDetail
		spendErrChan chan error
		reorgChan    chan struct{}
		spendCancel  context.CancelFunc
		confChan     chan *chainntnfs.TxConfirmation
		confErrChan  chan error
		confCancel   context.CancelFunc
		spendingTxID chainhash.Hash
	)
	cancelConfirmation := func() {
		if confCancel != nil {
			confCancel()
			confCancel = nil
		}
		confChan, confErrChan = nil, nil
		spendingTxID = chainhash.Hash{}
	}
	defer cancelConfirmation()

	// Replace both subscriptions after a reorg. The spend channel can still
	// contain the old spender, so retaining that stream could re-arm a
	// confirmation watch for an already reorged transaction. A fresh spend
	// subscription replays the current canonical spender, if there is one.
	registerSpend := func() error {
		cancelConfirmation()
		if spendCancel != nil {
			spendCancel()
		}
		var spendCtx context.Context
		spendCtx, spendCancel = context.WithCancel(watchCtx)
		reorgChan = make(chan struct{})

		var err error
		spendChan, spendErrChan, err =
			f.cfg.ChainNotifier.RegisterSpendNtfn(
				spendCtx, &f.deposit.OutPoint,
				f.deposit.AddressParams.PkScript,
				int32(f.deposit.GetConfirmationHeight()),
				lndclient.WithReOrgChan(reorgChan),
			)
		return err
	}
	if err := registerSpend(); err != nil {
		return f.HandleError(err)
	}

	for {
		select {
		case <-reorgChan:
			if err := registerSpend(); err != nil {
				return f.HandleError(err)
			}

		case err := <-spendErrChan:
			// lnd ends the stream normally once a spend is beyond its
			// reorg safety horizon. Keep a buffered spend or its active
			// confirmation watch, but never treat EOF as proof that
			// this deposit was swept.
			if errors.Is(err, io.EOF) &&
				(confChan != nil || len(spendChan) > 0) {

				spendErrChan = nil
				continue
			}

			log.Debugf("error while sweeping expired deposit: %v", err)
			return fsm.OnError

		case spend, ok := <-spendChan:
			if !ok || spend == nil || spend.SpendingTx == nil {
				return f.HandleError(errors.New("expiry spend notification " +
					"missing transaction"))
			}

			spendingTx := spend.SpendingTx
			if err := validateExpirySpend(
				spendingTx, f.deposit.OutPoint,
				f.deposit.TimeOutSweepPkScript,
			); err != nil {
				return f.HandleError(err)
			}

			cancelConfirmation()
			confCtx, cancel := context.WithCancel(watchCtx)
			confCancel = cancel
			txID := spendingTx.TxHash()
			spendingTxID = txID
			heightHint := spend.SpendingHeight
			if heightHint <= 0 {
				heightHint = int32(f.deposit.GetConfirmationHeight())
			}

			var err error
			confChan, confErrChan, err =
				f.cfg.ChainNotifier.RegisterConfirmationsNtfn(
					confCtx, &txID,
					f.deposit.TimeOutSweepPkScript,
					DefaultConfTarget, heightHint,
				)
			if err != nil {
				return f.HandleError(err)
			}

		case err := <-confErrChan:
			log.Debugf("error while confirming expired deposit: %v",
				err)
			return fsm.OnError

		case confirmation, ok := <-confChan:
			// A reorg and a confirmation may both be ready. Invalidate
			// the old watch before accepting an already queued result.
			select {
			case <-reorgChan:
				if err := registerSpend(); err != nil {
					return f.HandleError(err)
				}
				continue

			default:
			}

			if !ok || confirmation == nil || confirmation.Tx == nil {
				return f.HandleError(errors.New("expiry confirmation " +
					"missing transaction"))
			}

			confirmedTx := confirmation.Tx

			if confirmedTx.TxHash() != spendingTxID {
				return f.HandleError(fmt.Errorf("expiry confirmation " +
					"transaction does not match outpoint spender"))
			}
			err := validateExpirySpend(
				confirmedTx, f.deposit.OutPoint,
				f.deposit.TimeOutSweepPkScript,
			)
			if err != nil {
				return f.HandleError(err)
			}

			f.deposit.ExpirySweepTxid = spendingTxID
			return OnExpirySwept

		case <-ctx.Done():
			return fsm.OnError
		}
	}
}

// validateExpirySpend verifies that tx spends the deposit outpoint to the
// configured timeout sweep destination.
func validateExpirySpend(tx *wire.MsgTx, outpoint wire.OutPoint,
	timeoutPkScript []byte) error {

	spendsDeposit := false
	for _, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint == outpoint {
			spendsDeposit = true
			break
		}
	}
	if !spendsDeposit {
		return fmt.Errorf("expiry transaction does not spend deposit %v",
			outpoint)
	}

	for _, txOut := range tx.TxOut {
		if bytes.Equal(txOut.PkScript, timeoutPkScript) {
			return nil
		}
	}

	return errors.New("expiry transaction does not pay timeout sweep " +
		"destination")
}

// FinalizeDepositAction is the final action after a withdrawal. It signals to
// the manager that the deposit has been swept and the FSM can be removed.
func (f *FSM) FinalizeDepositAction(_ context.Context,
	_ fsm.EventContext) fsm.EventType {

	outpoint := f.deposit.OutPoint

	// The finalization notification only tells the manager to remove the
	// deposit from its active set. Send it asynchronously so a busy manager
	// loop can't stall withdrawal confirmation while deposit locks are held.
	go func() {
		select {
		case <-f.quitChan:
			// The deposit is already in a final state. If shutdown wins
			// this race, startup recovery will skip it instead of
			// re-adding it to the active set.
			return

		case f.finalizedDepositChan <- outpoint:
		}
	}()

	return fsm.NoOp
}
