package sweepbatcher

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ensurePresigned checks that there is a presigned transaction spending the
// inputs of this group only. If allowNonEmptyBatch is false, the batch must be
// empty.
func (b *batch) ensurePresigned(ctx context.Context, newSweeps []*sweep,
	allowNonEmptyBatch bool) error {

	if b.cfg.presignedHelper == nil {
		return fmt.Errorf("presignedHelper is not installed")
	}
	if len(b.sweeps) != 0 && !allowNonEmptyBatch {
		return fmt.Errorf("ensurePresigned should be done when " +
			"adding to an empty batch")
	}

	return ensurePresigned(
		ctx, newSweeps, b.cfg.presignedHelper, b.cfg.chainParams,
	)
}

// presignedTxChecker has methods to check if the inputs are presigned.
type presignedTxChecker interface {
	destPkScripter
	presigner
}

// ensurePresigned checks that there is a presigned transaction spending the
// inputs of this group only.
func ensurePresigned(ctx context.Context, newSweeps []*sweep,
	presignedTxChecker presignedTxChecker,
	chainParams *chaincfg.Params) error {

	sweeps := make([]sweep, len(newSweeps))
	for i, s := range newSweeps {
		sweeps[i] = sweep{
			outpoint:  s.outpoint,
			value:     s.value,
			presigned: s.presigned,
		}
	}

	// The sweeps are ordered inside the group, the first one is the primary
	// outpoint in the batch.
	primarySweepID := sweeps[0].outpoint

	// Cache the destination address.
	destAddr, err := getPresignedSweepsDestAddr(
		ctx, presignedTxChecker, primarySweepID, chainParams,
	)
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
	var batchAmt btcutil.Amount
	for _, sweep := range newSweeps {
		batchAmt += sweep.value
	}
	const loadOnly = true
	signedTx, err := presignedTxChecker.SignTx(
		ctx, primarySweepID, tx, batchAmt, feeRate, feeRate, loadOnly,
	)
	if err != nil {
		return fmt.Errorf("failed to find a presigned transaction "+
			"for feeRate %v, txid of the template is %v, inputs: %d, "+
			"outputs: %d: %w", feeRate, tx.TxHash(),
			len(tx.TxIn), len(tx.TxOut), err)
	}

	// Check the SignTx worked correctly.
	err = CheckSignedTx(tx, signedTx, batchAmt, feeRate)
	if err != nil {
		return fmt.Errorf("signed tx doesn't correspond the "+
			"unsigned tx: %w", err)
	}

	return nil
}

// getOrderedSweeps returns the sweeps of the batch in the order they were
// added. The method must be called from the event loop of the batch.
func (b *batch) getOrderedSweeps(ctx context.Context) ([]sweep, error) {
	// We use the DB just to know the order. Sweeps are copied from RAM.
	utxo2sweep := make(map[wire.OutPoint]sweep, len(b.sweeps))
	for _, s := range b.sweeps {
		utxo2sweep[s.outpoint] = s
	}

	dbSweeps, err := b.store.FetchBatchSweeps(ctx, b.id)
	if err != nil {
		return nil, fmt.Errorf("FetchBatchSweeps(%d) failed: %w", b.id,
			err)
	}
	dbSweeps = filterDbSweeps(b.cfg.skippedTxns, dbSweeps)

	if len(dbSweeps) != len(utxo2sweep) {
		return nil, fmt.Errorf("FetchBatchSweeps(%d) returned %d "+
			"sweeps, len(b.sweeps) is %d", b.id, len(dbSweeps),
			len(utxo2sweep))
	}

	orderedSweeps := make([]sweep, len(dbSweeps))
	for i, dbSweep := range dbSweeps {
		// Sanity check: make sure dbSweep.ID grows.
		if i > 0 && dbSweep.ID <= dbSweeps[i-1].ID {
			return nil, fmt.Errorf("sweep ID does not grow: %d->%d",
				dbSweeps[i-1].ID, dbSweep.ID)
		}

		s, has := utxo2sweep[dbSweep.Outpoint]
		if !has {
			return nil, fmt.Errorf("FetchBatchSweeps(%d) returned "+
				"unknown sweep %v", b.id, dbSweep.Outpoint)
		}
		orderedSweeps[i] = s
	}

	return orderedSweeps, nil
}

// getSweepsGroups returns groups in which sweeps were added to the batch.
// All the sweeps are sorted by addition order and grouped by swap.
// The method must be called from the event loop of the batch.
func (b *batch) getSweepsGroups(ctx context.Context) ([][]sweep, error) {
	orderedSweeps, err := b.getOrderedSweeps(ctx)
	if err != nil {
		return nil, fmt.Errorf("getOrderedSweeps(%d) failed: %w", b.id,
			err)
	}

	groups := [][]sweep{}
	for _, s := range orderedSweeps {
		index := len(groups) - 1

		// Start new group if there are no groups or new swap starts.
		if len(groups) == 0 || s.swapHash != groups[index][0].swapHash {
			groups = append(groups, []sweep{})
			index++
		}

		groups[index] = append(groups[index], s)
	}

	// Sanity check: make sure the number of groups is the same as the
	// number of distinct swaps.
	swapsSet := make(map[lntypes.Hash]struct{}, len(groups))
	for _, s := range orderedSweeps {
		swapsSet[s.swapHash] = struct{}{}
	}
	if len(swapsSet) != len(groups) {
		return nil, fmt.Errorf("batch %d: there are %d groups of "+
			"sweeps and %d distinct swaps", b.id, len(groups),
			len(swapsSet))
	}

	return groups, nil
}

// presign tries to presign batch sweep transactions composed of this batch and
// the sweep. In addition to that it presigns sweep transactions for any subset
// of sweeps that could remain if one of the sweep transactions gets confirmed.
// This can be done efficiently, since we keep track of the order in which
// sweeps are added and the associated swap hashes. So we presign transactions
// sweeping all the sweeps starting at some past sweeps group. For each inputs
// layout it presigns many transactions with different fee rates.
func (b *batch) presign(ctx context.Context, newSweeps []*sweep) error {
	if b.cfg.presignedHelper == nil {
		return fmt.Errorf("presignedHelper is not installed")
	}
	if len(b.sweeps) == 0 {
		return fmt.Errorf("presigning should be done when adding to " +
			"a non-empty batch")
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

	// We need to restore previously added groups. We can do it by reading
	// all the sweeps from DB (they must be ordered) and grouping by swap.
	groups, err := b.getSweepsGroups(ctx)
	if err != nil {
		return fmt.Errorf("getSweepsGroups failed: %w", err)
	}
	if len(groups) == 0 {
		return fmt.Errorf("getSweepsGroups returned no sweeps groups")
	}

	// Now presign a transaction spending a suffix of groups as well as new
	// sweeps. Any non-empty suffix of groups may remain non-swept after
	// some past tx is confirmed.
	for len(groups) != 0 {
		// Create the list of sweeps from the remaining groups and new
		// sweeps.
		sweeps := make([]sweep, 0, len(b.sweeps)+len(newSweeps))
		for _, group := range groups {
			sweeps = append(sweeps, group...)
		}
		for _, sweep := range newSweeps {
			sweeps = append(sweeps, *sweep)
		}

		// The primarySweepID is the first sweep from the list of
		// remaining sweeps if previous groups are confirmed.
		primarySweepID := sweeps[0].outpoint

		// Cache the destination address.
		destAddr, err := getPresignedSweepsDestAddr(
			ctx, b.cfg.presignedHelper, b.primarySweepID,
			b.cfg.chainParams,
		)
		if err != nil {
			return fmt.Errorf("failed to find destination "+
				"address: %w", err)
		}

		err = presign(
			ctx, b.cfg.presignedHelper, destAddr, primarySweepID,
			sweeps, nextBlockFeeRate,
		)
		if err != nil {
			return fmt.Errorf("failed to presign a transaction "+
				"of %d sweeps: %w", len(sweeps), err)
		}

		// Cut a group to proceed to next suffix of original groups.
		groups = groups[1:]
	}

	// Ensure that a batch spending new sweeps only has been presigned by
	// PresignSweepsGroup.
	const allowNonEmptyBatch = true
	err = b.ensurePresigned(ctx, newSweeps, allowNonEmptyBatch)
	if err != nil {
		return fmt.Errorf("new sweeps were not presigned; this means "+
			"that PresignSweepsGroup was not called prior to "+
			"AddSweep for the group: %w", err)
	}

	return nil
}

// presigner tries to presign a batch transaction.
type presigner interface {
	// SignTx signs an unsigned transaction or returns a pre-signed tx.
	// It is only called with loadOnly=true by ensurePresigned.
	SignTx(ctx context.Context, primarySweepID wire.OutPoint,
		tx *wire.MsgTx, inputAmt btcutil.Amount,
		minRelayFee, feeRate chainfee.SatPerKWeight,
		loadOnly bool) (*wire.MsgTx, error)
}

// presign tries to presign batch sweep transactions of the sweeps. It signs
// multiple versions of the transaction to make sure there is a transaction to
// be published if minRelayFee grows. If feerate is high, then a presigned tx
// gets LockTime equal to timeout minus 50 blocks, as a precautionary measure.
// A feerate is considered high if it is at least 100 sat/vbyte AND is at least
// 10x of the current next block feerate.
func presign(ctx context.Context, presigner presigner, destAddr btcutil.Address,
	primarySweepID wire.OutPoint, sweeps []sweep,
	nextBlockFeeRate chainfee.SatPerKWeight) error {

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
	// If timeout <= timeoutThreshold, don't set LockTime (keep value 0).
	var highFeeRateLockTime uint32
	if timeout > timeoutThreshold {
		highFeeRateLockTime = uint32(timeout - timeoutThreshold)
	}

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
		const (
			loadOnly    = false
			minRelayFee = chainfee.AbsoluteFeePerKwFloor
		)
		_, err = presigner.SignTx(
			ctx, primarySweepID, tx, batchAmt, minRelayFee, fr,
			loadOnly,
		)
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

	// Determine the current minimum relay fee based on our chain backend.
	minRelayFee, err := b.wallet.MinRelayFee(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get minRelayFee: %w", err),
			false
	}

	// Cache current height and desired feerate of the batch.
	currentHeight := b.currentHeight
	feeRate := max(b.rbfCache.FeeRate, minRelayFee)

	// Append this sweep to an array of sweeps. This is needed to keep the
	// order of sweeps stored, as iterating the sweeps map does not
	// guarantee same order.
	sweeps := make([]sweep, 0, len(b.sweeps))
	for _, sweep := range b.sweeps {
		sweeps = append(sweeps, sweep)
	}

	// Cache the destination address.
	address, err := getPresignedSweepsDestAddr(
		ctx, b.cfg.presignedHelper, b.primarySweepID,
		b.cfg.chainParams,
	)
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

	// Get a pre-signed transaction.
	const loadOnly = false
	signedTx, err := b.cfg.presignedHelper.SignTx(
		ctx, b.primarySweepID, tx, batchAmt, minRelayFee, feeRate,
		loadOnly,
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
		txHash, feeRate, signedFeeRate, realWeight, fee, numSweeps,
		address)
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

	// Update cached FeeRate not to broadcast a tx with lower feeRate.
	b.rbfCache.FeeRate = max(b.rbfCache.FeeRate, signedFeeRate)

	return fee, nil, true
}

// destPkScripter returns destination pkScript used by the sweep batch.
type destPkScripter interface {
	// DestPkScript returns destination pkScript used by the sweep batch
	// with the primary outpoint specified. Returns an error, if such tx
	// doesn't exist. If there are many such transactions, returns any of
	// pkScript's; all of them should have the same destination pkScript.
	DestPkScript(ctx context.Context,
		primarySweepID wire.OutPoint) ([]byte, error)
}

// getPresignedSweepsDestAddr returns the destination address used by the
// primary outpoint. The function must be used in presigned mode only.
func getPresignedSweepsDestAddr(ctx context.Context, helper destPkScripter,
	primarySweepID wire.OutPoint,
	chainParams *chaincfg.Params) (btcutil.Address, error) {

	// Load pkScript from the presigned helper.
	pkScriptBytes, err := helper.DestPkScript(ctx, primarySweepID)
	if err != nil {
		return nil, fmt.Errorf("presignedHelper.DestPkScript failed "+
			"for primarySweepID %v: %w", primarySweepID, err)
	}

	// Convert pkScript to btcutil.Address.
	pkScript, err := txscript.ParsePkScript(pkScriptBytes)
	if err != nil {
		return nil, fmt.Errorf("txscript.ParsePkScript failed for "+
			"pkScript %x returned for primarySweepID %v: %w",
			pkScriptBytes, primarySweepID, err)
	}

	address, err := pkScript.Address(chainParams)
	if err != nil {
		return nil, fmt.Errorf("pkScript.Address failed for "+
			"pkScript %x returned for primarySweepID %v: %w",
			pkScriptBytes, primarySweepID, err)
	}

	return address, nil
}

// CheckSignedTx makes sure that signedTx matches the unsignedTx. It checks
// according to criteria specified in the description of PresignedHelper.SignTx.
func CheckSignedTx(unsignedTx, signedTx *wire.MsgTx, inputAmt btcutil.Amount,
	minRelayFee chainfee.SatPerKWeight) error {

	// Make sure all inputs of signedTx have a non-empty witness.
	for _, txIn := range signedTx.TxIn {
		if len(txIn.Witness) == 0 {
			return fmt.Errorf("input %s of signed tx is not signed",
				txIn.PreviousOutPoint)
		}
	}

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
