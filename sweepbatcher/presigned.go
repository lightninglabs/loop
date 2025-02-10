package sweepbatcher

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ensurePresigned checks that we can sign a 1:1 transaction sweeping the input.
func (b *batch) ensurePresigned(ctx context.Context, newSweep *sweep) error {
	if b.cfg.presignedHelper == nil {
		return fmt.Errorf("presignedHelper is not installed")
	}
	if len(b.sweeps) != 0 {
		return fmt.Errorf("ensurePresigned is done when adding to an " +
			"empty batch")
	}

	sweeps := []sweep{
		{
			outpoint:  newSweep.outpoint,
			value:     newSweep.value,
			presigned: newSweep.presigned,
		},
	}

	// Cache the destination address.
	destAddr, err := b.getSweepsDestAddr(ctx, sweeps)
	if err != nil {
		return fmt.Errorf("failed to find destination address: %w", err)
	}

	// Set LockTime to 0. It is not critical.
	const currentHeight = 0

	// Check if we can sign with minimum fee rate.
	const feeRate = chainfee.FeePerKwFloor

	tx, _, _, _, err := constructUnsignedTx(
		sweeps, destAddr, currentHeight, feeRate,
	)
	if err != nil {
		return fmt.Errorf("failed to construct unsigned tx "+
			"for feeRate %v: %w", feeRate, err)
	}

	// Check of a presigned transaction exists.
	batchAmt := newSweep.value
	const presignedOnly = true
	signedTx, err := b.cfg.presignedHelper.SignTx(
		ctx, tx, batchAmt, feeRate, feeRate, presignedOnly,
	)
	if err != nil {
		return fmt.Errorf("failed to sign unsigned tx %v "+
			"for feeRate %v: %w", tx.TxHash(), feeRate, err)
	}

	// Check the SignTx worked correctly.
	err = CheckSignedTx(tx, signedTx, batchAmt, feeRate)
	if err != nil {
		return fmt.Errorf("signed tx doesn't correspond the "+
			"unsigned tx: %w", err)
	}

	return nil
}

// presign tries to presign batch sweep transactions composed of this batch and
// the sweep. It signs multiple versions of the transaction to make sure there
// is a transaction to be published if minRelayFee grows.
func (b *batch) presign(ctx context.Context, newSweep *sweep) error {
	if b.cfg.presignedHelper == nil {
		return fmt.Errorf("presignedHelper is not installed")
	}
	if len(b.sweeps) == 0 {
		return fmt.Errorf("presigning is done when adding to a " +
			"non-empty batch")
	}

	// priorityConfTarget defines the confirmation target for quick
	// inclusion in a block. A value of 2, rather than 1, is used to prevent
	// fee estimator from failing.
	// See https://github.com/lightninglabs/loop/issues/898
	const priorityConfTarget = 2

	// Find the feerate needed to get into next block.
	nextBlockFeeRate, err := b.wallet.EstimateFeeRate(
		ctx, priorityConfTarget,
	)
	if err != nil {
		return fmt.Errorf("failed to get nextBlockFeeRate: %w", err)
	}

	b.Infof("nextBlockFeeRate is %v", nextBlockFeeRate)

	// Create the list of sweeps of the future batch.
	sweeps := make([]sweep, 0, len(b.sweeps)+1)
	for _, sweep := range b.sweeps {
		sweeps = append(sweeps, sweep)
	}
	existingSweeps := sweeps
	sweeps = append(sweeps, *newSweep)

	// Cache the destination address.
	destAddr, err := b.getSweepsDestAddr(ctx, existingSweeps)
	if err != nil {
		return fmt.Errorf("failed to find destination address: %w", err)
	}

	return presign(
		ctx, b.cfg.presignedHelper, destAddr, sweeps, nextBlockFeeRate,
	)
}

// presigner tries to presign a batch transaction.
type presigner interface {
	// Presign tries to presign a batch transaction. If the method returns
	// nil, it is guaranteed that future calls to SignTx on this set of
	// sweeps return valid signed transactions.
	Presign(ctx context.Context, tx *wire.MsgTx,
		inputAmt btcutil.Amount) error
}

// presign tries to presign batch sweep transactions of the sweeps. It signs
// multiple versions of the transaction to make sure there is a transaction to
// be published if minRelayFee grows. If feerate is high, then a presigned tx
// gets LockTime equal to timeout minus 50 blocks, as a precautionary measure.
// A feerate is considered high if it is at least 100 sat/vbyte AND is at least
// 10x of the current next block feerate.
func presign(ctx context.Context, presigner presigner, destAddr btcutil.Address,
	sweeps []sweep, nextBlockFeeRate chainfee.SatPerKWeight) error {

	if presigner == nil {
		return fmt.Errorf("presigner is not installed")
	}

	if len(sweeps) == 0 {
		return fmt.Errorf("there are no sweeps")
	}

	if nextBlockFeeRate == 0 {
		return fmt.Errorf("nextBlockFeeRate is not set")
	}

	// Keep track of the total amount this batch is sweeping back.
	batchAmt := btcutil.Amount(0)
	for _, sweep := range sweeps {
		batchAmt += sweep.value
	}

	// Find the sweep with the earliest expiry.
	timeout := sweeps[0].timeout
	for _, sweep := range sweeps[1:] {
		timeout = min(timeout, sweep.timeout)
	}
	if timeout <= 0 {
		return fmt.Errorf("timeout is invalid: %d", timeout)
	}

	// Go from the floor (1.01 sat/vbyte) to 2k sat/vbyte with step of 1.2x.
	const (
		start            = chainfee.FeePerKwFloor
		stop             = chainfee.AbsoluteFeePerKwFloor * 2_000
		factorPPM        = 1_200_000
		timeoutThreshold = 50
	)

	// Calculate the locktime value to use for high feerate transactions.
	highFeeRateLockTime := uint32(timeout - timeoutThreshold)

	// Calculate which feerate to consider high. At least 100 sat/vbyte and
	// at least 10x of current nextBlockFeeRate.
	highFeeRate := max(100*chainfee.FeePerKwFloor, 10*nextBlockFeeRate)

	// Set LockTime to 0. It is not critical.
	const currentHeight = 0

	for fr := start; fr <= stop; fr = (fr * factorPPM) / 1_000_000 {
		// Construct an unsigned transaction for this fee rate.
		tx, _, feeForWeight, fee, err := constructUnsignedTx(
			sweeps, destAddr, currentHeight, fr,
		)
		if err != nil {
			return fmt.Errorf("failed to construct unsigned tx "+
				"for feeRate %v: %w", fr, err)
		}

		// If the feerate is high enough, set locktime to prevent
		// broadcasting such a transaction too early by mistake.
		if fr >= highFeeRate {
			tx.LockTime = highFeeRateLockTime
		}

		// Try to presign this transaction.
		err = presigner.Presign(ctx, tx, batchAmt)
		if err != nil {
			return fmt.Errorf("failed to presign unsigned tx %v "+
				"for feeRate %v: %w", tx.TxHash(), fr, err)
		}

		// If fee was clamped, stop here, because fee rate won't grow.
		if fee < feeForWeight {
			break
		}
	}

	return nil
}

// publishPresigned creates sweep transaction using a custom transaction signer
// and publishes it. It returns the fee of the transaction, and an error (if
// signing and/or publishing failed) and a boolean flag indicating signing
// success. This mode is incompatible with an external address.
func (b *batch) publishPresigned(ctx context.Context) (btcutil.Amount, error,
	bool) {

	// Sanity check, there should be at least 1 sweep in this batch.
	if len(b.sweeps) == 0 {
		return 0, fmt.Errorf("no sweeps in batch"), false
	}

	// Make sure that no external address is used.
	for _, sweep := range b.sweeps {
		if sweep.isExternalAddr {
			return 0, fmt.Errorf("external address was used with " +
				"a custom transaction signer"), false
		}
	}

	// Cache current height and desired feerate of the batch.
	currentHeight := b.currentHeight
	feeRate := b.rbfCache.FeeRate

	// Append this sweep to an array of sweeps. This is needed to keep the
	// order of sweeps stored, as iterating the sweeps map does not
	// guarantee same order.
	sweeps := make([]sweep, 0, len(b.sweeps))
	for _, sweep := range b.sweeps {
		sweeps = append(sweeps, sweep)
	}

	// Cache the destination address.
	address, err := b.getSweepsDestAddr(ctx, sweeps)
	if err != nil {
		return 0, fmt.Errorf("failed to find destination address: %w",
			err), false
	}

	// Construct unsigned batch transaction.
	tx, weight, _, fee, err := constructUnsignedTx(
		sweeps, address, currentHeight, feeRate,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to construct tx: %w", err),
			false
	}

	// Adjust feeRate, because it may have been clamped.
	feeRate = chainfee.NewSatPerKWeight(fee, weight)

	// Calculate total input amount.
	batchAmt := btcutil.Amount(0)
	for _, sweep := range sweeps {
		batchAmt += sweep.value
	}

	// Determine the current minimum relay fee based on our chain backend.
	minRelayFee, err := b.wallet.MinRelayFee(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get minRelayFee: %w", err),
			false
	}

	// Get a signed transaction. It may be either new transaction or a
	// pre-signed one.
	const presignedOnly = false
	signedTx, err := b.cfg.presignedHelper.SignTx(
		ctx, tx, batchAmt, minRelayFee, feeRate, presignedOnly,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to sign tx: %w", err),
			false
	}

	// Run sanity checks to make sure presignedHelper.SignTx complied with
	// all the invariants.
	err = CheckSignedTx(tx, signedTx, batchAmt, minRelayFee)
	if err != nil {
		return 0, fmt.Errorf("signed tx doesn't correspond the "+
			"unsigned tx: %w", err), false
	}
	tx = signedTx
	txHash := tx.TxHash()

	// Make sure tx weight matches the expected value.
	realWeight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(tx)),
	)
	if realWeight != weight {
		b.Warnf("actual weight of tx %v is %v, estimated as %d",
			txHash, realWeight, weight)
	}

	// Find actual fee rate of the signed transaction. It may differ from
	// the desired fee rate, because SignTx may return a presigned tx.
	output := btcutil.Amount(tx.TxOut[0].Value)
	fee = batchAmt - output
	signedFeeRate := chainfee.NewSatPerKWeight(fee, realWeight)

	numSweeps := len(tx.TxIn)
	b.Infof("attempting to publish custom signed tx=%v, desiredFeerate=%v,"+
		" signedFeeRate=%v, weight=%v, fee=%v, sweeps=%d, destAddr=%s",
		txHash, feeRate, signedFeeRate, weight, fee, numSweeps, address)
	b.debugLogTx("serialized batch", tx)

	// Publish the transaction.
	err = b.wallet.PublishTransaction(ctx, tx, b.cfg.txLabeler(b.id))
	if err != nil {
		return 0, fmt.Errorf("publishing tx failed: %w", err), true
	}

	// Store the batch transaction's txid and pkScript, for monitoring
	// purposes.
	b.batchTxid = &txHash
	b.batchPkScript = tx.TxOut[0].PkScript

	return fee, nil, true
}

// getSweepsDestAddr returns the destination address used by a group of sweeps.
// The method must be used in presigned mode only.
func (b *batch) getSweepsDestAddr(ctx context.Context,
	sweeps []sweep) (btcutil.Address, error) {

	if b.cfg.presignedHelper == nil {
		return nil, fmt.Errorf("getSweepsDestAddr used without " +
			"presigned mode")
	}

	inputs := make([]wire.OutPoint, len(sweeps))
	for i, s := range sweeps {
		if !s.presigned {
			return nil, fmt.Errorf("getSweepsDestAddr used on a " +
				"non-presigned input")
		}

		inputs[i] = s.outpoint
	}

	// Load pkScript from the presigned helper.
	pkScriptBytes, err := b.cfg.presignedHelper.DestPkScript(ctx, inputs)
	if err != nil {
		return nil, fmt.Errorf("presignedHelper.DestPkScript failed "+
			"for inputs %v: %w", inputs, err)
	}

	// Convert pkScript to btcutil.Address.
	pkScript, err := txscript.ParsePkScript(pkScriptBytes)
	if err != nil {
		return nil, fmt.Errorf("txscript.ParsePkScript failed for "+
			"pkScript %x returned for inputs %v: %w", pkScriptBytes,
			inputs, err)
	}

	address, err := pkScript.Address(b.cfg.chainParams)
	if err != nil {
		return nil, fmt.Errorf("pkScript.Address failed for "+
			"pkScript %x returned for inputs %v: %w", pkScriptBytes,
			inputs, err)
	}

	return address, nil
}

// CheckSignedTx makes sure that signedTx matches the unsignedTx. It checks
// according to criteria specified in the description of PresignedHelper.SignTx.
func CheckSignedTx(unsignedTx, signedTx *wire.MsgTx, inputAmt btcutil.Amount,
	minRelayFee chainfee.SatPerKWeight) error {

	// Make sure the set of inputs is the same.
	unsignedMap := make(map[wire.OutPoint]uint32, len(unsignedTx.TxIn))
	for _, txIn := range unsignedTx.TxIn {
		unsignedMap[txIn.PreviousOutPoint] = txIn.Sequence
	}
	for _, txIn := range signedTx.TxIn {
		seq, has := unsignedMap[txIn.PreviousOutPoint]
		if !has {
			return fmt.Errorf("input %s is new in signed tx",
				txIn.PreviousOutPoint)
		}
		if seq != txIn.Sequence {
			return fmt.Errorf("sequence mismatch in input %s: "+
				"%d in unsigned, %d in signed",
				txIn.PreviousOutPoint, seq, txIn.Sequence)
		}
		delete(unsignedMap, txIn.PreviousOutPoint)
	}
	for outpoint := range unsignedMap {
		return fmt.Errorf("input %s is missing in signed tx", outpoint)
	}

	// Compare outputs.
	if len(unsignedTx.TxOut) != 1 {
		return fmt.Errorf("unsigned tx has %d outputs, want 1",
			len(unsignedTx.TxOut))
	}
	if len(signedTx.TxOut) != 1 {
		return fmt.Errorf("the signed tx has %d outputs, want 1",
			len(signedTx.TxOut))
	}
	unsignedOut := unsignedTx.TxOut[0]
	signedOut := signedTx.TxOut[0]
	if !bytes.Equal(unsignedOut.PkScript, signedOut.PkScript) {
		return fmt.Errorf("mismatch of output pkScript: %v, %v",
			unsignedOut.PkScript, signedOut.PkScript)
	}

	// Find the feerate of signedTx.
	fee := inputAmt - btcutil.Amount(signedOut.Value)
	weight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(signedTx)),
	)
	feeRate := chainfee.NewSatPerKWeight(fee, weight)
	if feeRate < minRelayFee {
		return fmt.Errorf("feerate (%v) of signed tx is lower than "+
			"minRelayFee (%v)", feeRate, minRelayFee)
	}

	// Check LockTime.
	if signedTx.LockTime > unsignedTx.LockTime {
		return fmt.Errorf("locktime (%d) of signed tx is higher than "+
			"locktime of unsigned tx (%d)", signedTx.LockTime,
			unsignedTx.LockTime)
	}

	// Check Version.
	if signedTx.Version != unsignedTx.Version {
		return fmt.Errorf("version (%d) of signed tx is not equal to "+
			"version of unsigned tx (%d)", signedTx.Version,
			unsignedTx.Version)
	}

	return nil
}
